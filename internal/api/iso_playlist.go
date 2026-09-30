package api

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"golift.io/udf"
)

var errISOUnsupported = errors.New("ISO main-title metadata unavailable")

func isoUnsupported(reason string) error {
	return fmt.Errorf("%w: %s", errISOUnsupported, reason)
}

type isoPlayItem struct {
	Clip    string
	In, Out uint32
	Special bool
}

type isoPlaylist struct {
	Items    []isoPlayItem
	Duration uint64 // Blu-ray presentation clock ticks (45 kHz).
	Special  bool
}

// Parse only the playlist structure needed to reject incomplete or ambiguous
// main titles. Field layout follows libbluray's _parse_playlist/_parse_playitem:
// https://code.videolan.org/videolan/libbluray/-/blob/master/src/libbluray/bdnav/mpls_parse.c
func parseISOPlaylist(data []byte) (isoPlaylist, error) {
	var playlist isoPlaylist
	if len(data) < 20 || string(data[:4]) != "MPLS" ||
		(string(data[4:8]) != "0100" && string(data[4:8]) != "0200" && string(data[4:8]) != "0300") {
		return playlist, isoUnsupported("invalid MPLS header")
	}
	start := uint64(binary.BigEndian.Uint32(data[8:12]))
	if start < 20 || start+10 > uint64(len(data)) {
		return playlist, isoUnsupported("invalid playlist offset")
	}
	end := start + 4 + uint64(binary.BigEndian.Uint32(data[start:start+4]))
	if end < start+10 || end > uint64(len(data)) {
		return playlist, isoUnsupported("truncated playlist")
	}
	count := int(binary.BigEndian.Uint16(data[start+6 : start+8]))
	// Subpaths and extension data describe auxiliary disc features. This route
	// probes the primary title's tracks only; actual ISO playback stays with
	// Infuse, so those features must not change the player's source.
	if count == 0 || count > 1024 {
		return playlist, isoUnsupported("invalid playlist item count")
	}
	pos := start + 10
	for i := 0; i < count; i++ {
		if pos+2 > end {
			return playlist, isoUnsupported("missing playlist item")
		}
		length := uint64(binary.BigEndian.Uint16(data[pos : pos+2]))
		pos += 2
		if length < 32 || pos+length > end {
			return playlist, isoUnsupported("truncated playlist item")
		}
		item := data[pos : pos+length]
		if string(item[5:9]) != "M2TS" {
			return playlist, isoUnsupported("unsupported playlist clip type")
		}
		clip := string(item[:5])
		for _, digit := range clip {
			if digit < '0' || digit > '9' {
				return playlist, isoUnsupported("invalid playlist clip ID")
			}
		}
		entry := isoPlayItem{
			Clip: clip + ".m2ts", In: binary.BigEndian.Uint32(item[12:16]), Out: binary.BigEndian.Uint32(item[16:20]),
			Special: binary.BigEndian.Uint16(item[9:11])&0x10 != 0 || item[29] != 0,
		}
		if entry.Out <= entry.In {
			return playlist, isoUnsupported("invalid playlist timing")
		}
		playlist.Items = append(playlist.Items, entry)
		playlist.Duration += uint64(entry.Out - entry.In)
		playlist.Special = playlist.Special || entry.Special
		pos += length
	}
	return playlist, nil
}

// A dominant file alone does not prove that it is the complete movie. The
// longest playlist must use that single clip; another near-length title with
// different clips or timing makes automatic selection ambiguous.
func verifyISOPlaylists(playlists []isoPlaylist, mainClip string) error {
	if len(playlists) == 0 {
		return isoUnsupported("no Blu-ray playlists")
	}
	best := playlists[0]
	for _, playlist := range playlists[1:] {
		if playlist.Duration > best.Duration {
			best = playlist
		}
	}
	if len(best.Items) != 1 || best.Special || !strings.EqualFold(best.Items[0].Clip, mainClip) {
		return isoUnsupported(fmt.Sprintf("main playlist requires special handling (clips=%d, special=%v, target=%s, items=%v)", len(best.Items), best.Special, mainClip, best.Items))
	}
	for _, playlist := range playlists {
		if playlist.Duration*10 < best.Duration*9 {
			continue
		}
		if len(playlist.Items) != 1 || playlist.Special || playlist.Items[0] != best.Items[0] {
			return isoUnsupported("multiple main-title versions or timelines")
		}
	}
	return nil
}

func verifyISOMainPlaylist(bdmv []udf.File, mainClip string) error {
	dir := isoFindDir(bdmv, "PLAYLIST")
	if dir == nil {
		return isoUnsupported("BDMV/PLAYLIST directory missing")
	}
	entries, err := dir.ReadDir()
	if err != nil {
		return err
	}
	var playlists []isoPlaylist
	var totalBytes int64
	for _, file := range entries {
		if file.IsDir() || !strings.HasSuffix(strings.ToLower(file.Name()), ".mpls") {
			continue
		}
		size := file.Size()
		totalBytes += size
		if size <= 0 || size > 1<<20 || totalBytes > 8<<20 || len(playlists) >= 512 {
			return isoUnsupported("playlist metadata exceeds automatic probe limits")
		}
		reader, err := file.NewReader()
		if err != nil {
			return err
		}
		data, err := io.ReadAll(io.LimitReader(reader, size+1))
		if err != nil {
			return err
		}
		if int64(len(data)) != size {
			return io.ErrUnexpectedEOF
		}
		playlist, err := parseISOPlaylist(data)
		if err != nil {
			return err
		}
		playlists = append(playlists, playlist)
	}
	return verifyISOPlaylists(playlists, mainClip)
}
