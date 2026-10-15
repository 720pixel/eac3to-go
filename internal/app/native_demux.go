package app

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"

	"eac3to/internal/aften"
	"eac3to/internal/bitstream"
	"eac3to/internal/lpcm"
	"eac3to/internal/pgstiming"
	"eac3to/internal/seamless"
	"eac3to/internal/transport"
	"eac3to/internal/truehd"
	"eac3to/internal/video"
)

type nativeOutput struct {
	output Output
	file   *os.File
	writer *bufio.Writer
	video  *video.H264Writer
}

type nativeTrack struct {
	ctx               context.Context
	log               io.Writer
	pid               uint16
	codec             string
	parser            *seamless.Parser
	overlap           *seamless.Overlap
	outputs           []*nativeOutput
	frames            int64
	removed           int64
	bytes             int64
	firstPTS          int64
	sampleRate        int
	lpcm              *lpcm.Decoder
	pgs               *pgstiming.Assembler
	pgsTiming         []pgstiming.ClipTiming
	pgsStarts         []int64
	delayMS           int64
	residualMS        int64
	delayReady        bool
	skipFrames        int64
	initialCut        int64
	initialPad        int64
	checkCRC          bool
	dtsBuffer         []byte
	dtsCoreBuffer     []byte
	truehdParser      *truehd.Parser
	truehdPost        *truehd.Postprocessor
	truehdBuffer      []byte
	truehdChanged     int64
	truehdAtmosNotice bool
	truehdCompanion   bool
}

func nativeCopySupported(out Output) bool {
	if !out.Copy || out.CacheCodec != "" || out.Track.Stream == nil {
		return false
	}
	switch out.Track.Stream.Codec {
	case "pcm_bluray":
		return out.NativePCM
	case "ac3", "eac3":
		return out.BSF == "" && out.Format == out.Track.Stream.Codec
	case "dts":
		return out.Format == "dts" && (out.BSF == "" || out.BSF == "dca_core")
	case "truehd":
		return out.Format == "truehd" && out.BSF == ""
	case "h264", "hevc", "mpeg2video", "vc1":
		return out.Format == out.Track.Stream.Codec && out.BSF == ""
	case "hdmv_pgs_subtitle":
		return out.Format == "sup" && out.BSF == ""
	}
	return false
}

func sharedTrueHDPID(s *Source, stream *Stream) bool {
	if stream == nil || stream.Codec != "ac3" && stream.Codec != "eac3" || stream.ID == "" {
		return false
	}
	pid, err := strconv.ParseUint(stream.ID, 0, 13)
	if err != nil {
		return false
	}
	for _, other := range s.Probe.Streams {
		if other.Codec == "truehd" {
			otherPID, err := strconv.ParseUint(other.ID, 0, 13)
			if err == nil && otherPID == pid {
				return true
			}
		}
	}
	return false
}

func copyNativeStreams(ctx context.Context, s *Source, outputs []Output, o Options, log io.Writer) ([]Output, error) {
	if !nativeTransportSource(s) {
		return outputs, nil
	}
	var chosen, remaining []Output
	for _, out := range outputs {
		companion := out.TrueHDCompanion || sharedTrueHDPID(s, out.Track.Stream)
		if s.Playlist == nil && out.Codec != "truehd" && !companion {
			remaining = append(remaining, out)
			continue
		}
		if nativeCopySupported(out) {
			chosen = append(chosen, out)
		} else {
			remaining = append(remaining, out)
		}
	}
	if len(chosen) == 0 {
		return outputs, nil
	}
	scans, err := sourceNativeScans(ctx, s, o)
	if err != nil {
		return nil, err
	}
	var timing initialTransportTiming
	for _, out := range chosen {
		if out.Track.Stream.Type == "audio" || out.Track.Stream.Type == "subtitle" {
			timing, err = sourceNativeTiming(ctx, s, scans)
			if err != nil {
				return nil, err
			}
			break
		}
	}
	type trackKey struct {
		pid       uint16
		delay     int64
		codec     string
		companion bool
	}
	groups := map[trackKey]*nativeTrack{}
	codecs := map[uint16]string{}
	var files []*nativeOutput
	defer func() {
		for _, out := range files {
			out.file.Close()
		}
	}()
	for _, out := range chosen {
		st := out.Track.Stream
		value, err := strconv.ParseUint(st.ID, 0, 13)
		if err != nil || value >= 0x1fff {
			return nil, fmt.Errorf("track %d has no valid transport PID", out.Track.Number)
		}
		pid := uint16(value)
		delay := timing.Delays[pid] + int64(out.Request.Options.DelayMS)
		if st.Codec == "truehd" && out.TrueHDPrepared {
			delay = out.TrueHDDelayMS
		}
		if out.PCMSourceDelay != nil {
			// Cache the original samples so opposite source and manual delays cancel.
			*out.PCMSourceDelay = timing.Delays[pid]
			delay = 0
		}
		companion := out.TrueHDCompanion || sharedTrueHDPID(s, st)
		key := trackKey{pid: pid, delay: delay, codec: st.Codec, companion: companion}
		track := groups[key]
		if track == nil {
			track = &nativeTrack{ctx: ctx, log: log, pid: pid, codec: st.Codec, firstPTS: transport.NoTimestamp, delayMS: delay, checkCRC: !o.Fast, truehdCompanion: companion}
			groups[key] = track
		}
		if previous := codecs[pid]; previous != "" && previous != st.Codec && !((previous == "truehd" && companion) || ((previous == "ac3" || previous == "eac3") && st.Codec == "truehd")) {
			return nil, fmt.Errorf("PID 0x%04x has conflicting codecs", pid)
		}
		codecs[pid] = st.Codec
		file, err := os.Create(out.Stage)
		if err != nil {
			return nil, err
		}
		native := &nativeOutput{output: out, file: file, writer: bufio.NewWriterSize(file, 1<<20)}
		files = append(files, native)
		if videoTimingRequested(out) {
			native.video, err = newVideoTimingWriter(out, native.writer)
			if err != nil {
				return nil, err
			}
		}
		track.outputs = append(track.outputs, native)
	}
	tracks := make([]*nativeTrack, 0, len(groups))
	handlers := map[uint16]func(transport.PES) error{}
	byPID := map[uint16][]*nativeTrack{}
	for key, track := range groups {
		tracks = append(tracks, track)
		byPID[key.pid] = append(byPID[key.pid], track)
		if track.codec == "truehd" {
			if err := track.setupTrueHD(len(scans) > 1); err != nil {
				return nil, err
			}
		}
		if track.codec == "hdmv_pgs_subtitle" {
			if err := track.setupPGS(scans, timing, o); err != nil {
				return nil, err
			}
		}
		if track.codec == "pcm_bluray" {
			track.lpcm, err = lpcm.New(lpcm.Config{})
			if err != nil {
				return nil, err
			}
		}
		if track.codec == "ac3" || track.codec == "eac3" || track.codec == "dts" {
			codec := seamless.AC3
			if track.codec == "dts" {
				codec = seamless.DTS
			}
			const frameLimit = (1 << 20) + 16384 + 3
			if !track.truehdCompanion {
				track.overlap, err = seamless.NewOverlap(frameLimit)
				if err != nil {
					return nil, err
				}
			}
			track.parser, err = seamless.NewParser(seamless.Config{Codec: codec, MaxFrameBytes: frameLimit, OnFrame: track.frame})
			if err != nil {
				return nil, err
			}
		}
	}
	for pid, pidTracks := range byPID {
		var thdTracks, coreTracks []*nativeTrack
		shared := false
		for _, track := range pidTracks {
			shared = shared || track.codec == "truehd" || track.truehdCompanion
		}
		if shared {
			for _, track := range pidTracks {
				if track.codec == "truehd" {
					thdTracks = append(thdTracks, track)
				} else if track.truehdCompanion {
					coreTracks = append(coreTracks, track)
				} else {
					return nil, fmt.Errorf("PID 0x%04x has unsupported TrueHD companions", pid)
				}
			}
			cfg := truehd.Config{PID: pid}
			if len(thdTracks) > 0 {
				cfg.OnTrueHD = func(pes transport.PES) error {
					for _, track := range thdTracks {
						if err := track.pes(pes); err != nil {
							return err
						}
					}
					return nil
				}
			}
			if len(coreTracks) > 0 {
				cfg.OnAC3 = func(pes transport.PES) error {
					for _, track := range coreTracks {
						if err := track.pes(pes); err != nil {
							return err
						}
					}
					return nil
				}
			}
			separator, err := truehd.New(cfg)
			if err != nil {
				return nil, err
			}
			handlers[pid] = separator.WritePES
		} else {
			handlers[pid] = func(pes transport.PES) error {
				for _, track := range pidTracks {
					if err := track.pes(pes); err != nil {
						return err
					}
				}
				return nil
			}
		}
	}
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].pid != tracks[j].pid {
			return tracks[i].pid < tracks[j].pid
		}
		if tracks[i].codec != tracks[j].codec {
			return tracks[i].codec < tracks[j].codec
		}
		return tracks[i].delayMS < tracks[j].delayMS
	})
	windowed := map[uint16]bool{}
	if o.PlaylistVideoWindow && s.Playlist != nil {
		for _, out := range chosen {
			if out.Track.Kind == "video" {
				pid, _ := strconv.ParseUint(out.Track.Stream.ID, 0, 13)
				windowed[uint16(pid)] = true
			}
		}
	}
	demux, err := transport.New(transport.Config{
		Handlers:   handlers,
		Strict:     o.StrictPL,
		StrictPIDs: windowed,
		OnBoundary: func(boundary transport.Boundary) error {
			if !boundary.Before {
				return nil
			}
			for _, track := range tracks {
				if track.parser != nil && track.overlap != nil {
					if err := track.overlap.Boundary(seamless.WindowsBoundary(track.parser.PendingBytes())); err != nil {
						return err
					}
				}
			}
			return nil
		},
	})
	if err != nil {
		return nil, err
	}
	if !o.Quiet {
		fmt.Fprintf(log, "Extracting %d outputs from %d clips with the native transport reader.\n", len(chosen), len(scans))
	}
	progress := newExtractionProgress(scans, log, o.Quiet)
	for i, clip := range scans {
		file, err := os.Open(clip.Path)
		if err != nil {
			return nil, err
		}
		segment := transport.Segment{Index: i, PacketSize: clip.Scan.PacketSize, HasWindow: s.Playlist != nil, In90k: clip.In90k, Out90k: clip.Out90k, BoundaryDelta90k: clip.Boundary}
		progress.clip = i + 1
		err = demux.ReadSegment(ctx, progress.reader(file), segment)
		closeErr := file.Close()
		if err != nil {
			return nil, fmt.Errorf("clip %s: %w", clip.Clip, err)
		}
		if closeErr != nil {
			return nil, closeErr
		}
	}
	if err := demux.Finish(); err != nil {
		return nil, err
	}
	progress.finish()
	for _, track := range tracks {
		if track.truehdParser != nil {
			if err := track.finishTrueHD(); err != nil {
				return nil, fmt.Errorf("PID 0x%04x TrueHD tail: %w", track.pid, err)
			}
			if !o.Quiet {
				stats := track.truehdPost.Stats()
				fmt.Fprintf(log, "PID 0x%04x: %d TrueHD access units, %d Windows postprocess units suppressed, %d dialogue headers changed; delay %+d ms carried in output names.\n", track.pid, track.frames, stats.SuppressedFrames, track.truehdChanged, track.delayMS)
			}
		}
		if track.parser != nil {
			if err := track.parser.Flush(); err != nil {
				return nil, fmt.Errorf("PID 0x%04x audio tail: %w", track.pid, err)
			}
		}
		if track.pgs != nil {
			if err := track.pgs.Finish(); err != nil {
				return nil, fmt.Errorf("PID 0x%04x subtitle tail: %w", track.pid, err)
			}
			track.bytes = track.pgs.Stats().OutputBytes
			if !o.Quiet {
				fmt.Fprintf(log, "PID 0x%04x: %d subtitle segments.\n", track.pid, track.pgs.Stats().Segments)
			}
		}
		if track.bytes == 0 {
			return nil, fmt.Errorf("PID 0x%04x has no output payload", track.pid)
		}
		if !o.Quiet && track.parser != nil {
			fmt.Fprintf(log, "PID 0x%04x: %d audio frames, %d identical boundary frames removed.\n", track.pid, track.frames, track.removed)
			if track.delayMS != 0 {
				fmt.Fprintf(log, "PID 0x%04x: source delay %+d ms; removed %d / inserted %d initial frames; remaining delay %+d ms.\n", track.pid, track.delayMS, track.initialCut, track.initialPad, track.residualMS)
			}
		}
		if !o.Quiet && track.lpcm != nil {
			fmt.Fprintf(log, "PID 0x%04x: %d PCM frames, source delay %+d ms; removed %d / inserted %d initial sample frames.\n", track.pid, track.frames, track.delayMS, track.initialCut, track.initialPad)
		}
	}
	for _, out := range files {
		if out.video != nil {
			if err := out.video.Finish(); err != nil {
				return nil, fmt.Errorf("track %d video timing: %w", out.output.Track.Number, err)
			}
			if !o.Quiet {
				logVideoTiming(log, out.output, out.video.Stats())
			}
		}
		if err := out.writer.Flush(); err != nil {
			return nil, err
		}
		if err := out.file.Close(); err != nil {
			return nil, err
		}
		if out.output.Forced {
			if err := rewrite(out.output.Stage, func(r io.Reader, w io.Writer) error {
				stats, err := bitstream.FilterForcedPGS(r, w)
				if err == nil && !o.Quiet {
					fmt.Fprintf(log, "PGS: %d forced objects in %d presentations.\n", stats.ForcedObjects, stats.Presentations)
				}
				return err
			}); err != nil {
				return nil, err
			}
		}
	}
	return remaining, nil
}

func (t *nativeTrack) pes(pes transport.PES) error {
	if t.firstPTS == transport.NoTimestamp && pes.PTS != transport.NoTimestamp {
		t.firstPTS = pes.PTS
	}
	if t.truehdParser != nil {
		return t.truehdParser.WritePES(pes)
	}
	if t.parser != nil {
		_, err := t.parser.Write(pes.Payload)
		return err
	}
	if t.pgs != nil {
		return t.writePGS(pes)
	}
	if t.lpcm != nil {
		result, err := t.lpcm.Decode(pes.Payload)
		if err != nil {
			return err
		}
		for _, out := range t.outputs {
			stream := out.output.Track.Stream
			if strconv.Itoa(result.Format.Rate) != stream.SampleRate || result.Format.Channels != stream.Channels || result.Format.StorageBits != storageDepth(sampleDepth(stream)) || stream.Layout != "" && result.Format.Layout != stream.Layout {
				return fmt.Errorf("LPCM packet format disagrees with the inspected track: %+v", result.Format)
			}
		}
		return t.writePCM(result)
	}
	for _, out := range t.outputs {
		var sink io.Writer = out.writer
		if out.video != nil {
			sink = out.video
		}
		if _, err := sink.Write(pes.Payload); err != nil {
			return err
		}
	}
	t.bytes += int64(len(pes.Payload))
	return nil
}

func (t *nativeTrack) frame(frame seamless.Frame) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	if t.codec == "ac3" && frame.Enhanced {
		return fmt.Errorf("AC-3 input changed to E-AC-3")
	}
	if t.checkCRC && (t.codec == "ac3" || t.codec == "eac3") {
		if err := bitstream.ValidateAudioFrame(frame.Bytes); err != nil {
			return err
		}
	}
	if t.sampleRate != 0 && frame.SampleRate != t.sampleRate {
		return fmt.Errorf("audio sample rate changed from %d to %d Hz", t.sampleRate, frame.SampleRate)
	}
	t.sampleRate = frame.SampleRate
	keep := true
	var err error
	if t.overlap != nil {
		keep, err = t.overlap.Accept(frame)
		if err != nil {
			return err
		}
	}
	if !keep {
		t.removed++
		return nil
	}
	if !t.delayReady {
		t.delayReady = true
		count, residual, err := compressedDelayFrames(t.codec, frame, t.delayMS)
		if err != nil {
			return err
		}
		t.residualMS = residual
		for _, out := range t.outputs {
			setOutputResidual(out.output, residual)
		}
		if count < 0 {
			t.skipFrames = -count
		} else if count > 0 {
			padding := frame
			if t.codec == "ac3" {
				data, err := aften.SilenceFromAC3Frame(frame.Bytes)
				if err == nil {
					padding.Bytes = data
				} else if t.log != nil {
					fmt.Fprintf(t.log, "PID 0x%04x: AC-3 silence generation failed (%v); repeating the first frame for the delay.\n", t.pid, err)
				}
			} else if t.codec != "dts" {
				return fmt.Errorf("positive %s delay is not implemented", t.codec)
			}
			for range count {
				if err := t.writeFrame(padding); err != nil {
					return err
				}
				t.initialPad++
			}
		}
	}
	if t.skipFrames > 0 {
		t.skipFrames--
		t.initialCut++
		return nil
	}
	return t.writeFrame(frame)
}

func (t *nativeTrack) writeFrame(frame seamless.Frame) error {
	if err := t.ctx.Err(); err != nil {
		return err
	}
	data := frame.Bytes
	repair := false
	for _, out := range t.outputs {
		repair = repair || !out.output.DecodeCache
	}
	if t.codec == "dts" && repair {
		if cap(t.dtsCoreBuffer) < len(data) {
			t.dtsCoreBuffer = make([]byte, len(data))
		}
		t.dtsCoreBuffer = t.dtsCoreBuffer[:len(data)]
		copy(t.dtsCoreBuffer, data)
		if _, err := bitstream.RepairDTSCoreResolution(t.dtsCoreBuffer); err != nil {
			return err
		}
		data = t.dtsCoreBuffer
	}
	base := data
	var normalized []byte
	for _, out := range t.outputs {
		data := base
		if out.output.DecodeCache {
			data = frame.Bytes
		}
		if out.output.RemoveDialnorm && t.codec == "dts" {
			if normalized == nil {
				if cap(t.dtsBuffer) < len(data) {
					t.dtsBuffer = make([]byte, len(data))
				}
				t.dtsBuffer = t.dtsBuffer[:len(data)]
				copy(t.dtsBuffer, data)
				if _, err := bitstream.RemoveDTSDialnormFrame(t.dtsBuffer); err != nil {
					return err
				}
				normalized = t.dtsBuffer
			}
			data = normalized
		}
		if out.output.BSF == "dca_core" {
			data = data[:frame.CoreBytes]
		}
		if out.output.RemoveDialnorm && t.codec != "dts" {
			if _, err := bitstream.RemoveDialnorm(bytes.NewReader(data), out.writer); err != nil {
				return err
			}
		} else if _, err := out.writer.Write(data); err != nil {
			return err
		}
	}
	t.frames++
	t.bytes += int64(len(frame.Bytes))
	return nil
}

func (t *nativeTrack) writePCM(result lpcm.Result) error {
	stride := result.Format.Channels * result.Format.StorageBits / 8
	if !t.delayReady {
		t.delayReady = true
		frames := t.delayMS * int64(result.Format.Rate/100) / 10
		if frames < 0 {
			t.skipFrames = -frames
		} else {
			zero := make([]byte, 4096*stride)
			for frames > 0 {
				if err := t.ctx.Err(); err != nil {
					return err
				}
				count := min(frames, 4096)
				for _, out := range t.outputs {
					if _, err := out.writer.Write(zero[:count*int64(stride)]); err != nil {
						return err
					}
				}
				frames -= count
				t.initialPad += count
				t.frames += count
				t.bytes += count * int64(stride)
			}
		}
	}
	skip := min(t.skipFrames, int64(result.Frames))
	t.skipFrames -= skip
	t.initialCut += skip
	data := result.PCM[skip*int64(stride):]
	for _, out := range t.outputs {
		if _, err := out.writer.Write(data); err != nil {
			return err
		}
	}
	t.frames += int64(result.Frames) - skip
	t.bytes += int64(len(data))
	return nil
}

func compressedDelayFrames(codec string, frame seamless.Frame, delayMS int64) (frames, residual int64, err error) {
	if delayMS == 0 {
		return 0, 0, nil
	}
	sign := int64(1)
	if delayMS < 0 {
		sign = -1
	}
	switch codec {
	case "ac3", "eac3":
		header, err := bitstream.ParseAudioHeader(frame.Bytes)
		if err != nil {
			return 0, 0, err
		}
		bitrate, size := int64(header.BitRate), int64(header.Size)
		if bitrate <= 0 || size <= 0 {
			return 0, 0, fmt.Errorf("cannot determine compressed audio frame duration")
		}
		bytes := delayMS * bitrate / 8000
		frames = (bytes + sign*(size/2)) / size
		return frames, delayMS - frames*size*8000/bitrate, nil
	case "dts":
		rate, samples := int64(frame.SampleRate/100), int64(frame.Samples)
		if rate <= 0 || samples <= 0 {
			return 0, 0, fmt.Errorf("cannot determine DTS frame duration")
		}
		frames = (delayMS*rate + sign*samples*5) / (samples * 10)
		return frames, delayMS - frames*samples*10/rate, nil
	default:
		return 0, 0, fmt.Errorf("compressed delay is unsupported for %s", codec)
	}
}
