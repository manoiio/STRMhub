using System;
using System.Collections.Generic;
using System.IO;
using System.Linq;
using System.Reflection;
using System.Text.RegularExpressions;
using System.Threading;
using System.Threading.Tasks;
using MediaBrowser.Common.Plugins;
using MediaBrowser.Controller.Api;
using MediaBrowser.Controller.Entities;
using MediaBrowser.Controller.Library;
using MediaBrowser.Controller.MediaEncoding;
using MediaBrowser.Controller.Net;
using MediaBrowser.Controller.Persistence;
using MediaBrowser.Model.Dto;
using MediaBrowser.Model.Entities;
using MediaBrowser.Model.Services;
using MediaBrowser.Model.MediaInfo;
using MediaBrowser.Model.Dlna;

[assembly: AssemblyVersion("1.0.0.0")]
[assembly: AssemblyFileVersion("1.0.0.0")]

namespace STRMhub.IsoMediaInfo
{
    public sealed class Plugin : BasePlugin
    {
        public override string Name { get { return "STRMhub ISO Media Info"; } }
        public override string Description { get { return "Imports primary-title media information while keeping original ISO playback URLs."; } }
        public override Guid Id { get { return new Guid("76214360-3926-4fd8-b5cd-660bb12a32f4"); } }
    }

    [Route("/STRMhub/IsoMediaInfo", "POST")]
    [Authenticated(Roles = "Admin")]
    public sealed class ProbeIso : IReturn<ProbeResult>
    {
        public long Id { get; set; }
        public string ProbeUrl { get; set; }
        public long IsoSize { get; set; }
    }

    public sealed class ProbeResult
    {
        public long ItemId { get; set; }
        public int VideoStreams { get; set; }
        public int AudioStreams { get; set; }
        public int SubtitleStreams { get; set; }
        public long? RunTimeTicks { get; set; }
        public string Container { get; set; }
    }

    public sealed class IsoMediaInfoService : BaseApiService
    {
        private readonly ILibraryManager _library;
        private readonly IItemRepository _repository;
        private readonly IMediaProbeManager _probe;
        private static readonly Regex ProbePath = new Regex(@"^/iso-media/([A-Za-z0-9]{8,64})/main\.m2ts$", RegexOptions.CultureInvariant);
        private static readonly Regex DirectPath = new Regex(@"^/d/([A-Za-z0-9]{8,64})(?:\.iso)?$", RegexOptions.IgnoreCase | RegexOptions.CultureInvariant);

        public IsoMediaInfoService(ILibraryManager library, IItemRepository repository, IMediaProbeManager probe)
        {
            _library = library;
            _repository = repository;
            _probe = probe;
        }

        public object Post(ProbeIso request)
        {
            return Import(request).GetAwaiter().GetResult();
        }

        private async Task<ProbeResult> Import(ProbeIso request)
        {
            var video = _library.GetItemById(request.Id) as Video;
            if (video == null || string.IsNullOrEmpty(video.Path) || !video.Path.EndsWith(".iso.strm", StringComparison.OrdinalIgnoreCase))
                throw new ArgumentException("Item must be an existing ISO STRM video.");
            Uri probeUri;
            if (!Uri.TryCreate(request.ProbeUrl, UriKind.Absolute, out probeUri) || probeUri.Scheme != "http" ||
                probeUri.Host != "127.0.0.1" || !string.IsNullOrEmpty(probeUri.Query) || !string.IsNullOrEmpty(probeUri.Fragment))
                throw new ArgumentException("Probe URL must use the local STRMhub metadata endpoint.");
            var probeMatch = ProbePath.Match(probeUri.AbsolutePath);
            if (!probeMatch.Success || request.IsoSize <= 0)
                throw new ArgumentException("Invalid metadata source.");
            var fileInfo = new FileInfo(video.Path);
            if (fileInfo.Length > 16384)
                throw new ArgumentException("Invalid STRM file.");
            var original = File.ReadLines(video.Path).FirstOrDefault();
            Uri originalUri;
            if (!Uri.TryCreate((original ?? "").Trim(), UriKind.Absolute, out originalUri))
                throw new ArgumentException("Invalid original ISO URL.");
            var originalMatch = DirectPath.Match(originalUri.AbsolutePath);
            if (!originalMatch.Success || originalMatch.Groups[1].Value != probeMatch.Groups[1].Value)
                throw new ArgumentException("Probe does not match the item's original ISO.");

            using (var timeout = new CancellationTokenSource(TimeSpan.FromSeconds(110)))
            {
                // This MediaSourceInfo is temporary and is never assigned to
                // the item or its STRM. Infuse keeps the original ISO source.
                var info = await _probe.GetMediaInfo(new MediaInfoRequest
                {
                    MediaType = DlnaProfileType.Video,
                    MediaSource = new MediaSourceInfo
                    {
                        Path = request.ProbeUrl,
                        Protocol = MediaProtocol.Http,
                        Container = "mpegts",
                        IsRemote = true
                    }
                }, timeout.Token).ConfigureAwait(false);
                var streams = new List<MediaStream>(info.MediaStreams ?? new List<MediaStream>());
                var primaryVideo = streams.FirstOrDefault(stream => stream.Type == MediaStreamType.Video);
                if (primaryVideo == null || string.IsNullOrEmpty(primaryVideo.Codec))
                    throw new InvalidOperationException("Primary title has no valid video stream.");
                // Preserve externally attached subtitles and avoid index
                // collisions with the freshly probed embedded tracks.
                int nextIndex = streams.Count == 0 ? 0 : streams.Max(stream => stream.Index) + 1;
                foreach (var external in video.GetMediaStreams().Where(stream => stream.IsExternal))
                {
                    external.Index = nextIndex++;
                    streams.Add(external);
                }
                _repository.SaveMediaStreams(video.InternalId, streams, timeout.Token);
                video.Container = "blurayiso";
                video.RunTimeTicks = info.RunTimeTicks;
                video.Size = request.IsoSize;
                video.Width = primaryVideo.Width ?? 0;
                video.Height = primaryVideo.Height ?? 0;
                video.TotalBitrate = info.Bitrate ?? 0;
                video.UpdateToRepository(ItemUpdateType.MetadataImport);
                return new ProbeResult
                {
                    ItemId = video.InternalId,
                    VideoStreams = streams.Count(stream => stream.Type == MediaStreamType.Video),
                    AudioStreams = streams.Count(stream => stream.Type == MediaStreamType.Audio),
                    SubtitleStreams = streams.Count(stream => stream.Type == MediaStreamType.Subtitle),
                    RunTimeTicks = info.RunTimeTicks,
                    Container = video.Container
                };
            }
        }
    }
}
