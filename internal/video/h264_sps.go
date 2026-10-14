// Package video edits elementary-stream metadata without decoding pictures.
// Write to a temporary destination: malformed input found late in a stream can
// leave partial output, which must not be published.
package video

import (
	"fmt"
	"io"
	"math"
)

// MaxH264SPSSize bounds the encoded size of one sequence parameter set.
const MaxH264SPSSize = 64 << 10

// H264Clock is the VUI clock. Its nominal frame rate is TimeScale divided by
// twice NumUnitsInTick; picture timing and field structure can alter cadence.
type H264Clock struct {
	NumUnitsInTick uint32
	TimeScale      uint32
}

func (c H264Clock) FPS() float64 {
	if c.NumUnitsInTick == 0 {
		return 0
	}
	return float64(c.TimeScale) / (2 * float64(c.NumUnitsInTick))
}

func (c H264Clock) valid() bool { return c.NumUnitsInTick != 0 && c.TimeScale != 0 }

// H264ClockForFPS uses the 1000/1001 clock convention for the supported film
// and television rates. Rounded decimal forms such as 23.976 are accepted.
func H264ClockForFPS(fps float64) (H264Clock, error) {
	if math.IsNaN(fps) || math.IsInf(fps, 0) || fps <= 0 || fps > 120 {
		return H264Clock{}, fmt.Errorf("unsupported H.264 frame rate %g", fps)
	}
	code := int(math.Round(fps * 1000))
	var rate, tick uint32
	switch code {
	case 23976, 24975, 29970, 47952, 59940:
		rate = uint32((code*1001 + 500000) / 1000000)
		tick = 1001
	case 24000, 25000, 30000, 48000, 50000, 60000:
		rate = uint32(code / 1000)
		tick = 1000
	default:
		return H264Clock{}, fmt.Errorf("unsupported H.264 frame rate %g", fps)
	}
	return H264Clock{NumUnitsInTick: tick, TimeScale: rate * 2000}, nil
}

// H264SPS describes the original sequence parameter set. Clock is zero when
// TimingPresent is false. FixedFrameRate is the original flag or its inferred
// false value when timing information was absent.
type H264SPS struct {
	ID                  uint32
	ProfileIDC          uint8
	LevelIDC            uint8
	ChromaFormatIDC     uint8
	BitDepthLuma        uint8
	BitDepthChroma      uint8
	SeparateColourPlane bool
	FrameMbsOnly        bool
	VUIPresent          bool
	TimingPresent       bool
	Clock               H264Clock
	FixedFrameRate      bool
	PicStructPresent    bool
}

type spsLayout struct {
	info       H264SPS
	rbsp       []byte
	vuiFlag    int
	timingFlag int
	clockStart int
	payloadEnd int
}

// ParseH264SPS validates one SPS NAL, including its one-byte NAL header but
// excluding Annex B start codes and trailing_zero_8bits. It does not mutate nal.
func ParseH264SPS(nal []byte) (H264SPS, error) {
	layout, err := parseSPS(nal)
	return layout.info, err
}

// RewriteH264SPS replaces the VUI clock or adds missing timing information.
// Existing SPS fields, including fixed_frame_rate_flag, are preserved. Added
// timing uses the previously inferred false fixed-frame-rate flag.
func RewriteH264SPS(nal []byte, clock H264Clock) ([]byte, H264SPS, error) {
	if !clock.valid() {
		return nil, H264SPS{}, fmt.Errorf("H.264 timing clocks must be nonzero")
	}
	layout, err := parseSPS(nal)
	if err != nil {
		return nil, layout.info, err
	}
	if layout.info.TimingPresent && layout.info.Clock == clock {
		return append([]byte(nil), nal...), layout.info, nil
	}
	w := bitBuilder{data: make([]byte, 0, len(layout.rbsp)+16)}
	if layout.info.TimingPresent {
		w.copy(layout.rbsp, 0, layout.clockStart)
		w.put(clock.NumUnitsInTick, 32)
		w.put(clock.TimeScale, 32)
		w.copy(layout.rbsp, layout.clockStart+64, layout.payloadEnd)
	} else if layout.info.VUIPresent {
		w.copy(layout.rbsp, 0, layout.timingFlag)
		w.put(1, 1)
		w.put(clock.NumUnitsInTick, 32)
		w.put(clock.TimeScale, 32)
		w.put(0, 1)
		w.copy(layout.rbsp, layout.timingFlag+1, layout.payloadEnd)
	} else {
		w.copy(layout.rbsp, 0, layout.vuiFlag)
		w.put(1, 1)
		w.put(0, 4) // Aspect ratio, overscan, video signal and chroma location.
		w.put(1, 1)
		w.put(clock.NumUnitsInTick, 32)
		w.put(clock.TimeScale, 32)
		w.put(0, 5) // Fixed rate, two HRD flags, picture structure and restrictions.
	}
	w.put(1, 1)
	for w.bits%8 != 0 {
		w.put(0, 1)
	}
	result := make([]byte, 1, len(nal)+16)
	result[0] = nal[0]
	zeros := 0
	for _, b := range w.data {
		if zeros == 2 && b <= 3 {
			result = append(result, 3)
			zeros = 0
		}
		result = append(result, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	return result, layout.info, nil
}

// Syntax follows ITU-T H.264 sections 7.3.2.1 and E.1. The field layout can
// also be checked in FFmpeg's libavcodec/cbs_h264_syntax_template.c.
func parseSPS(nal []byte) (spsLayout, error) {
	l := spsLayout{timingFlag: -1, clockStart: -1}
	if len(nal) < 2 {
		return l, fmt.Errorf("truncated H.264 SPS: %w", io.ErrUnexpectedEOF)
	}
	if len(nal) > MaxH264SPSSize {
		return l, fmt.Errorf("H.264 SPS exceeds %d bytes", MaxH264SPSSize)
	}
	if nal[0]&0x80 != 0 || nal[0]&31 != 7 || nal[0]&0x60 == 0 {
		return l, fmt.Errorf("invalid H.264 SPS NAL header 0x%02x", nal[0])
	}
	rbsp := make([]byte, 0, len(nal)-1)
	zeros := 0
	for i := 1; i < len(nal); i++ {
		b := nal[i]
		if zeros == 2 {
			if b == 3 {
				if i+1 == len(nal) || nal[i+1] > 3 {
					return l, fmt.Errorf("invalid H.264 SPS emulation-prevention byte")
				}
				zeros = 0
				continue
			}
			if b <= 2 {
				return l, fmt.Errorf("unescaped H.264 SPS start-code sequence")
			}
		}
		rbsp = append(rbsp, b)
		if b == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	l.rbsp = rbsp
	b := spsBits{data: rbsp}
	l.info.ProfileIDC = uint8(b.read(8))
	constraints := b.read(8)
	if constraints&3 != 0 {
		b.fail("nonzero SPS reserved bits")
	}
	l.info.LevelIDC = uint8(b.read(8))
	l.info.ID = b.ue("seq_parameter_set_id", 31)
	l.info.ChromaFormatIDC, l.info.BitDepthLuma, l.info.BitDepthChroma = 1, 8, 8
	switch l.info.ProfileIDC {
	case 66, 77, 88:
	case 44, 83, 86, 100, 110, 118, 122, 128, 138, 244:
		l.info.ChromaFormatIDC = uint8(b.ue("chroma_format_idc", 3))
		if l.info.ChromaFormatIDC == 3 {
			l.info.SeparateColourPlane = b.read(1) != 0
		}
		l.info.BitDepthLuma = uint8(b.ue("bit_depth_luma_minus8", 6)) + 8
		l.info.BitDepthChroma = uint8(b.ue("bit_depth_chroma_minus8", 6)) + 8
		b.read(1)
		if b.read(1) != 0 {
			lists := 8
			if l.info.ChromaFormatIDC == 3 {
				lists = 12
			}
			for i := 0; i < lists; i++ {
				if b.read(1) != 0 {
					n := 16
					if i >= 6 {
						n = 64
					}
					last, next := int32(8), int32(8)
					for j := 0; j < n; j++ {
						if next != 0 {
							delta := b.se("delta_scale")
							if delta < -128 || delta > 127 {
								b.fail("delta_scale outside [-128,127]")
							}
							next = (last + delta + 256) % 256
						}
						if next != 0 {
							last = next
						}
					}
				}
			}
		}
	default:
		b.fail("unsupported SPS profile %d", l.info.ProfileIDC)
	}
	b.ue("log2_max_frame_num_minus4", 12)
	poc := b.ue("pic_order_cnt_type", 2)
	if poc == 0 {
		b.ue("log2_max_pic_order_cnt_lsb_minus4", 12)
	} else if poc == 1 {
		b.read(1)
		b.se("offset_for_non_ref_pic")
		b.se("offset_for_top_to_bottom_field")
		n := b.ue("num_ref_frames_in_pic_order_cnt_cycle", 255)
		for i := uint32(0); i < n; i++ {
			b.se("offset_for_ref_frame")
		}
	}
	b.ue("max_num_ref_frames", 16)
	b.read(1)
	width := uint64(b.ue("pic_width_in_mbs_minus1", 65535)+1) * 16
	height := uint64(b.ue("pic_height_in_map_units_minus1", 65535)+1) * 16
	l.info.FrameMbsOnly = b.read(1) != 0
	if !l.info.FrameMbsOnly {
		height *= 2
		b.read(1)
	}
	b.read(1)
	if b.read(1) != 0 {
		left := uint64(b.ue("frame_crop_left_offset", math.MaxUint32-1))
		right := uint64(b.ue("frame_crop_right_offset", math.MaxUint32-1))
		top := uint64(b.ue("frame_crop_top_offset", math.MaxUint32-1))
		bottom := uint64(b.ue("frame_crop_bottom_offset", math.MaxUint32-1))
		unitX, unitY := uint64(1), uint64(1)
		if !l.info.SeparateColourPlane && l.info.ChromaFormatIDC != 0 {
			if l.info.ChromaFormatIDC != 3 {
				unitX = 2
			}
			if l.info.ChromaFormatIDC == 1 {
				unitY = 2
			}
		}
		if !l.info.FrameMbsOnly {
			unitY *= 2
		}
		if (left+right)*unitX >= width || (top+bottom)*unitY >= height {
			b.fail("SPS cropping removes the complete picture")
		}
	}
	l.vuiFlag = b.pos
	l.info.VUIPresent = b.read(1) != 0
	if l.info.VUIPresent {
		parseVUI(&b, &l)
	}
	l.payloadEnd = b.pos
	if b.read(1) != 1 {
		b.fail("missing SPS rbsp_stop_one_bit")
	}
	for b.err == nil && b.pos%8 != 0 {
		if b.read(1) != 0 {
			b.fail("nonzero SPS alignment bit")
		}
	}
	if b.err == nil && b.pos != len(rbsp)*8 {
		b.fail("unexpected bytes after SPS trailing bits")
	}
	if b.err != nil {
		return l, fmt.Errorf("invalid H.264 SPS: %w", b.err)
	}
	return l, nil
}

func parseVUI(b *spsBits, l *spsLayout) {
	if b.read(1) != 0 {
		aspect := b.read(8)
		if aspect == 255 {
			b.read(16)
			b.read(16)
		} else if aspect > 16 {
			b.fail("reserved aspect_ratio_idc %d", aspect)
		}
	}
	if b.read(1) != 0 {
		b.read(1)
	}
	if b.read(1) != 0 {
		if b.read(3) > 5 {
			b.fail("reserved video_format")
		}
		b.read(1)
		if b.read(1) != 0 {
			b.read(24)
		}
	}
	if b.read(1) != 0 {
		b.ue("chroma_sample_loc_type_top_field", 5)
		b.ue("chroma_sample_loc_type_bottom_field", 5)
	}
	l.timingFlag = b.pos
	l.info.TimingPresent = b.read(1) != 0
	if l.info.TimingPresent {
		l.clockStart = b.pos
		l.info.Clock.NumUnitsInTick = b.read(32)
		l.info.Clock.TimeScale = b.read(32)
		l.info.FixedFrameRate = b.read(1) != 0
		if !l.info.Clock.valid() {
			b.fail("zero SPS timing clock")
		}
	}
	nalHRD := b.read(1) != 0
	if nalHRD {
		parseHRD(b)
	}
	vclHRD := b.read(1) != 0
	if vclHRD {
		parseHRD(b)
	}
	if nalHRD || vclHRD {
		b.read(1)
	}
	l.info.PicStructPresent = b.read(1) != 0
	if b.read(1) != 0 {
		b.read(1)
		b.ue("max_bytes_per_pic_denom", 16)
		b.ue("max_bits_per_mb_denom", 16)
		b.ue("log2_max_mv_length_horizontal", 16)
		b.ue("log2_max_mv_length_vertical", 16)
		b.ue("max_num_reorder_frames", 16)
		b.ue("max_dec_frame_buffering", 16)
	}
}

func parseHRD(b *spsBits) {
	n := b.ue("cpb_cnt_minus1", 31)
	b.read(8)
	for i := uint32(0); i <= n; i++ {
		b.ue("bit_rate_value_minus1", math.MaxUint32-1)
		b.ue("cpb_size_value_minus1", math.MaxUint32-1)
		b.read(1)
	}
	b.read(20)
}

type spsBits struct {
	data []byte
	pos  int
	err  error
}

func (b *spsBits) fail(format string, args ...any) {
	if b.err == nil {
		b.err = fmt.Errorf(format, args...)
	}
}

func (b *spsBits) read(n int) uint32 {
	if b.err != nil {
		return 0
	}
	if n < 0 || n > 32 || n > len(b.data)*8-b.pos {
		b.err = io.ErrUnexpectedEOF
		return 0
	}
	var value uint32
	for ; n > 0; n-- {
		value = value<<1 | uint32((b.data[b.pos/8]>>uint(7-b.pos%8))&1)
		b.pos++
	}
	return value
}

func (b *spsBits) ue(name string, maximum uint32) uint32 {
	zeros := 0
	for b.err == nil && b.read(1) == 0 {
		zeros++
		if zeros > 31 {
			b.fail("%s Exp-Golomb value overflows", name)
		}
	}
	if b.err != nil {
		return 0
	}
	value := uint32((uint64(1)<<uint(zeros))-1) + b.read(zeros)
	if value > maximum {
		b.fail("%s exceeds %d", name, maximum)
		return 0
	}
	return value
}

func (b *spsBits) se(name string) int32 {
	u := b.ue(name, math.MaxUint32-1)
	if u&1 != 0 {
		return int32(u/2 + 1)
	}
	return -int32(u / 2)
}

type bitBuilder struct {
	data []byte
	bits int
}

func (b *bitBuilder) put(value uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if b.bits%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[b.bits/8] |= byte((value>>uint(i))&1) << uint(7-b.bits%8)
		b.bits++
	}
}

func (b *bitBuilder) copy(data []byte, start, end int) {
	for i := start; i < end; i++ {
		b.put(uint32((data[i/8]>>uint(7-i%8))&1), 1)
	}
}
