package api

import (
	"encoding/binary"
	"errors"
	"testing"
)

func isoPlaylistFixture(items []isoPlayItem) []byte {
	data := make([]byte, 40+10+34*len(items))
	copy(data, "MPLS0200")
	binary.BigEndian.PutUint32(data[8:12], 40)
	binary.BigEndian.PutUint32(data[40:44], uint32(len(data)-44))
	binary.BigEndian.PutUint16(data[46:48], uint16(len(items)))
	pos := 50
	for _, item := range items {
		binary.BigEndian.PutUint16(data[pos:pos+2], 32)
		payload := data[pos+2 : pos+34]
		copy(payload[:5], item.Clip[:5])
		copy(payload[5:9], "M2TS")
		if item.Special {
			binary.BigEndian.PutUint16(payload[9:11], 0x10)
		}
		binary.BigEndian.PutUint32(payload[12:16], item.In)
		binary.BigEndian.PutUint32(payload[16:20], item.Out)
		pos += 34
	}
	return data
}

func TestISOPlaylistPrimaryTitle(t *testing.T) {
	item := isoPlayItem{Clip: "00003.m2ts", In: 27000000, Out: 344116800}
	data := isoPlaylistFixture([]isoPlayItem{item})
	// Auxiliary metadata on a real disc must not force a playback substitute.
	binary.BigEndian.PutUint16(data[48:50], 1)
	binary.BigEndian.PutUint32(data[16:20], 1)
	playlist, err := parseISOPlaylist(data)
	if err != nil || playlist.Duration != 317116800 || len(playlist.Items) != 1 || playlist.Items[0] != item {
		t.Fatalf("primary title = %+v, err=%v", playlist, err)
	}
	if err := verifyISOPlaylists([]isoPlaylist{playlist, playlist}, "00003.m2ts"); err != nil {
		t.Fatal(err)
	}
}

func TestISOPlaylistRejectsTruncatedOrInvalidData(t *testing.T) {
	valid := isoPlaylistFixture([]isoPlayItem{{Clip: "00003.m2ts", Out: 45000}})
	cases := map[string]func([]byte) []byte{
		"truncated":         func(data []byte) []byte { return data[:len(data)-1] },
		"offset overflow":   func(data []byte) []byte { binary.BigEndian.PutUint32(data[8:12], 0xffffffff); return data },
		"invalid timing":    func(data []byte) []byte { binary.BigEndian.PutUint32(data[64:68], 45001); return data },
		"invalid clip":      func(data []byte) []byte { data[52] = '/'; return data },
		"unsupported codec": func(data []byte) []byte { copy(data[57:61], "FMTS"); return data },
	}
	for name, corrupt := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := parseISOPlaylist(corrupt(append([]byte(nil), valid...))); !errors.Is(err, errISOUnsupported) {
				t.Fatalf("invalid playlist accepted: %v", err)
			}
		})
	}
}

func TestISOPlaylistRejectsWrongOrIncompleteMainTitle(t *testing.T) {
	main := isoPlayItem{Clip: "00003.m2ts", Out: 3600 * 45000}
	menu := isoPlayItem{Clip: "00001.m2ts", Out: 60 * 45000}
	parsed := func(items ...isoPlayItem) isoPlaylist {
		result, err := parseISOPlaylist(isoPlaylistFixture(items))
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	cases := []struct {
		name      string
		playlists []isoPlaylist
		wantErr   bool
	}{
		{"single main and short menu", []isoPlaylist{parsed(menu), parsed(main)}, false},
		{"missing playlists", nil, true},
		{"longest needs two clips", []isoPlaylist{parsed(main, menu)}, true},
		{"another main version", []isoPlaylist{parsed(main), parsed(isoPlayItem{Clip: "00004.m2ts", Out: main.Out})}, true},
		{"different cut", []isoPlaylist{parsed(main), parsed(isoPlayItem{Clip: main.Clip, In: 45000, Out: main.Out})}, true},
		{"angle selection", []isoPlaylist{parsed(isoPlayItem{Clip: main.Clip, Out: main.Out, Special: true})}, true},
		{"dominant file is not longest title", []isoPlaylist{parsed(menu)}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifyISOPlaylists(tc.playlists, main.Clip)
			if (err != nil) != tc.wantErr {
				t.Fatalf("main selection error = %v", err)
			}
		})
	}
}
