package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

type audioJob struct {
	source *Source
	output Output
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func concurrency(o Options, count int) (jobs, threads int) {
	budget := o.Threads
	if budget == 0 {
		budget = runtime.GOMAXPROCS(0)
	}
	jobs = o.Jobs
	if jobs == 0 {
		jobs = min(4, budget)
	}
	jobs = max(1, min(jobs, count, budget))
	// Each job runs a decoder and encoder beside its Go processing loop.
	threads = max(1, budget/(3*jobs))
	return jobs, threads
}

func prepareAudioJobs(s *Source, outputs []Output) ([]audioJob, []Output, error) {
	var jobs []audioJob
	media := 0
	for _, out := range outputs {
		if out.Track.Kind != "chapters" {
			media++
		}
		if out.Track.Kind == "audio" && !out.Copy {
			if err := validateAudioOutput(out); err != nil {
				return nil, nil, err
			}
			jobs = append(jobs, audioJob{s, out})
		}
	}
	if media < 2 && s.Playlist == nil {
		return jobs, nil, nil
	}
	type cachedTrack struct {
		source *Source
		stream *Stream
	}
	cached := map[int]cachedTrack{}
	var caches []Output
	for i := range jobs {
		job := &jobs[i]
		st := job.output.Track.Stream
		nativePCM := s.Playlist != nil && st.Codec == "pcm_bluray"
		nativeFrames := s.Playlist != nil && (st.Codec == "dts" || st.Codec == "ac3")
		if media < 2 && !nativePCM && !nativeFrames {
			continue
		}
		cache, exists := cached[st.Index]
		if !exists {
			codec := ""
			switch st.Codec {
			case "ac3", "eac3", "dts", "truehd", "mlp", "flac":
			case "pcm_bluray", "pcm_dvd":
				depth := storageDepth(sampleDepth(st))
				if depth != 16 && depth != 24 && depth != 32 {
					return nil, nil, fmt.Errorf("cannot cache %s at %d bits without a precision change", st.Codec, depth)
				}
				codec = "pcm_s" + strconv.Itoa(depth) + "le"
			default:
				if !strings.HasPrefix(st.Codec, "pcm_") {
					continue
				}
			}
			dir, err := os.MkdirTemp(filepath.Dir(job.output.Stage), "cache-")
			if err != nil {
				return nil, nil, err
			}
			path := filepath.Join(dir, fmt.Sprintf("source-%d.mka", st.Index))
			cacheOut := Output{Track: job.output.Track, Copy: true, Format: "matroska", Codec: st.Codec, Path: path, Stage: path, CacheCodec: codec, DecodeCache: true}
			stream := *st
			stream.Index = 0
			input := []string{"-i", path}
			var sourceDelay *int64
			if nativePCM {
				sourceDelay = new(int64)
				cacheOut.PCMSourceDelay = sourceDelay
				path = filepath.Join(dir, fmt.Sprintf("source-%d.pcm", st.Index))
				cacheOut.Path, cacheOut.Stage = path, path
				cacheOut.Format = fmt.Sprintf("s%dle", storageDepth(sampleDepth(st)))
				cacheOut.Codec, cacheOut.CacheCodec, cacheOut.NativePCM = codec, "", true
				stream.Codec = codec
				input = []string{"-f", cacheOut.Format, "-ar", st.SampleRate, "-ac", strconv.Itoa(st.Channels)}
				if st.Layout != "" {
					input = append(input, "-channel_layout", st.Layout)
				}
				input = append(input, "-i", path)
			} else if nativeFrames {
				path = filepath.Join(dir, fmt.Sprintf("source-%d.%s", st.Index, st.Codec))
				cacheOut.Path, cacheOut.Stage = path, path
				cacheOut.Format = st.Codec
				input = []string{"-f", st.Codec, "-i", path}
			}
			caches = append(caches, cacheOut)
			cache = cachedTrack{source: &Source{Paths: []string{path}, InputArgs: input, Name: s.Name, PCMSourceDelay: sourceDelay}, stream: &stream}
			cached[st.Index] = cache
		}
		job.source = cache.source
		job.output.Track.Stream = cache.stream
	}
	return jobs, caches, nil
}

func processAudioJobs(ctx context.Context, t Tools, jobs []audioJob, o Options, log io.Writer) error {
	if len(jobs) == 0 {
		return nil
	}
	workers, threads := concurrency(o, len(jobs))
	o.CodecThreads = threads
	if !o.Quiet {
		fmt.Fprintf(log, "Processing %d audio outputs with %d workers (%d codec threads per process).\n", len(jobs), workers, threads)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	queue := make(chan audioJob)
	var wg sync.WaitGroup
	var once sync.Once
	var firstErr error
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for job := range queue {
				if ctx.Err() != nil {
					return
				}
				if !o.Quiet {
					fmt.Fprintf(log, "Processing track %d: %s\n", job.output.Track.Number, filepath.Base(job.output.Path))
				}
				if err := runAudio(ctx, t, job.source, job.output, o, log); err != nil {
					once.Do(func() { firstErr = fmt.Errorf("track %d: %w", job.output.Track.Number, err); cancel() })
					return
				}
			}
		}()
	}
loop:
	for _, job := range jobs {
		select {
		case queue <- job:
		case <-ctx.Done():
			break loop
		}
	}
	close(queue)
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}
