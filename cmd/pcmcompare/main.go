package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"

	"eac3to/internal/compare"
)

func main() { os.Exit(run(os.Args[1:])) }

func run(args []string) int {
	flags := flag.NewFlagSet("pcmcompare", flag.ContinueOnError)
	jsonOutput := flags.Bool("json", false, "write a JSON report")
	floatMode := flags.Bool("float", false, "compare exact float64 PCM representations")
	ffmpeg := flags.String("ffmpeg", "", "FFmpeg executable path")
	ffprobe := flags.String("ffprobe", "", "ffprobe executable path")
	refStream := flags.Int("reference-stream", -1, "zero-based reference audio-stream ordinal")
	canStream := flags.Int("candidate-stream", -1, "zero-based candidate audio-stream ordinal")
	flags.Usage = func() {
		fmt.Fprintln(flags.Output(), "Usage: pcmcompare [options] reference candidate\nExit status: 0 exact match, 1 differences, 2 failure.\nFrames and channels in reports are zero-based.")
		flags.PrintDefaults()
	}
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return 2
	}
	if flags.NArg() != 2 {
		flags.Usage()
		return 2
	}
	options := compare.Options{FFmpeg: *ffmpeg, FFprobe: *ffprobe, Float: *floatMode}
	if *refStream >= 0 {
		options.ReferenceStream = refStream
	}
	if *canStream >= 0 {
		options.CandidateStream = canStream
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	report, err := compare.Files(ctx, flags.Arg(0), flags.Arg(1), options)
	if err != nil {
		if *jsonOutput {
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"error": err.Error()})
		} else {
			fmt.Fprintln(os.Stderr, "pcmcompare:", err)
		}
		return 2
	}
	if *jsonOutput {
		encoder := json.NewEncoder(os.Stdout)
		encoder.SetIndent("", "  ")
		if err := encoder.Encode(report); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
	} else {
		status := "different"
		if report.Exact {
			status = "exact match"
		}
		fmt.Printf("%s (%s), %d Hz, %d channels, layout %q\n", status, report.Format, report.Reference.Audio.SampleRate, report.Reference.Audio.Channels, report.Reference.Audio.ChannelLayout)
		fmt.Printf("Reference: %d frames / %d samples\nCandidate: %d frames / %d samples\nDiffering samples: %d (missing: %d)\n", report.Reference.Frames, report.Reference.Samples, report.Candidate.Frames, report.Candidate.Samples, report.DifferingSamples, report.MissingSamples)
		if report.FirstDifference != nil {
			d := report.FirstDifference
			ref, can := "EOF", "EOF"
			if d.ReferenceValue != nil {
				ref = *d.ReferenceValue
			}
			if d.CandidateValue != nil {
				can = *d.CandidateValue
			}
			fmt.Printf("First difference: frame %d, channel %d; reference %s, candidate %s\n", d.Frame, d.Channel, ref, can)
		}
		if report.MaxIntegerError != nil {
			fmt.Printf("Maximum integer error: %d (normalized s32 units)\n", *report.MaxIntegerError)
		}
		fmt.Printf("FFmpeg: %s\n  %s\nffprobe: %s\n  %s\n", report.FFmpeg.Path, report.FFmpeg.Version, report.FFprobe.Path, report.FFprobe.Version)
	}
	if report.Exact {
		return 0
	}
	return 1
}
