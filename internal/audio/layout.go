package audio

import (
	"fmt"
	"math"
	"strings"
)

// Named native layouts use FFmpeg/WAVE speaker order, rather than codec
// bitstream order. The decoder must supply PCM in this order.
// https://github.com/FFmpeg/FFmpeg/blob/master/libavutil/channel_layout.h
var namedLayouts = map[string]string{
	"mono":           "FC",
	"stereo":         "FL+FR",
	"2.1":            "FL+FR+LFE",
	"3.0":            "FL+FR+FC",
	"3.0(back)":      "FL+FR+BC",
	"3.1":            "FL+FR+FC+LFE",
	"4.0":            "FL+FR+FC+BC",
	"quad":           "FL+FR+BL+BR",
	"quad(side)":     "FL+FR+SL+SR",
	"4.1":            "FL+FR+FC+LFE+BC",
	"5.0":            "FL+FR+FC+BL+BR",
	"5.0(side)":      "FL+FR+FC+SL+SR",
	"5.1":            "FL+FR+FC+LFE+BL+BR",
	"5.1(side)":      "FL+FR+FC+LFE+SL+SR",
	"6.0":            "FL+FR+FC+BC+SL+SR",
	"6.0(front)":     "FL+FR+FLC+FRC+SL+SR",
	"hexagonal":      "FL+FR+FC+BL+BR+BC",
	"6.1":            "FL+FR+FC+LFE+BC+SL+SR",
	"6.1(back)":      "FL+FR+FC+LFE+BL+BR+BC",
	"6.1(front)":     "FL+FR+LFE+FLC+FRC+SL+SR",
	"7.0":            "FL+FR+FC+BL+BR+SL+SR",
	"7.0(front)":     "FL+FR+FC+FLC+FRC+SL+SR",
	"7.1":            "FL+FR+FC+LFE+BL+BR+SL+SR",
	"7.1(wide)":      "FL+FR+FC+LFE+BL+BR+FLC+FRC",
	"7.1(wide-side)": "FL+FR+FC+LFE+FLC+FRC+SL+SR",
	"octagonal":      "FL+FR+FC+BL+BR+BC+SL+SR",
}

// WaveChannelMask requires WAVE speaker order; a mask alone cannot describe a
// different sample order. Unknown multichannel layouts must be assigned first.
func WaveChannelMask(layout string, channels int) (uint32, error) {
	roles, _, err := parseLayout(layout, channels)
	if err != nil {
		return 0, err
	}
	if len(roles) != channels {
		return 0, fmt.Errorf("encoding requires known speaker positions for %d channels", channels)
	}
	speakers := [...]string{"FL", "FR", "FC", "LFE", "BL", "BR", "FLC", "FRC", "BC", "SL", "SR", "TC", "TFL", "TFC", "TFR", "TBL", "TBC", "TBR"}
	var mask uint32
	previous := -1
	for _, role := range roles {
		position := -1
		for i, name := range speakers {
			if role == name {
				position = i
				break
			}
		}
		if position < 0 {
			return 0, fmt.Errorf("speaker %s has no WAVE channel-mask position", role)
		}
		if position <= previous {
			return 0, fmt.Errorf("layout %q does not use WAVE speaker order", layout)
		}
		mask |= uint32(1) << position
		previous = position
	}
	return mask, nil
}

func parseLayout(name string, channels int) ([]string, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "unknown" {
		switch channels {
		case 1:
			name = "mono"
		case 2:
			name = "stereo"
		default:
			return nil, "", nil
		}
	}
	value, known := namedLayouts[name]
	if !known {
		value = name
		if i := strings.Index(value, " channels ("); i != -1 && strings.HasSuffix(value, ")") {
			value = value[i+11 : len(value)-1]
		}
		if !strings.Contains(value, "+") {
			// Unrecognized layouts can still pass through without guessing
			// their speaker assignments.
			return nil, name, nil
		}
	}
	roles := strings.Split(value, "+")
	if len(roles) != channels {
		return nil, "", fmt.Errorf("layout %q has %d channels, input has %d", name, len(roles), channels)
	}
	seen := make(map[string]bool, len(roles))
	for i, role := range roles {
		role = strings.TrimSpace(role)
		if role == "" || seen[role] {
			return nil, "", fmt.Errorf("layout %q contains empty or repeated speaker names", name)
		}
		roles[i], seen[role] = role, true
	}
	if !known {
		name = strings.Join(roles, "+")
	}
	return roles, name, nil
}

func mixMatrix(cfg Config, roles []string, layout string) ([][]contribution, string, error) {
	if cfg.ExtractChannel >= 0 {
		return [][]contribution{{{cfg.ExtractChannel, 1}}}, "mono", nil
	}
	if cfg.Mix == "" {
		rows := make([][]contribution, cfg.Channels)
		for ch := range rows {
			rows[ch] = []contribution{{ch, 1}}
		}
		return rows, layout, nil
	}
	if len(roles) == 0 {
		return nil, "", fmt.Errorf("mixing requires known speaker positions for %d-channel input", cfg.Channels)
	}
	index := make(map[string]int, len(roles))
	for i, role := range roles {
		index[role] = i
	}
	if cfg.Mix == "mono" {
		if ch, ok := index["FC"]; ok {
			return [][]contribution{{{ch, 1}}}, "mono", nil
		}
		if ch, ok := index["FL"]; ok {
			return [][]contribution{{{ch, 1}}}, "mono", nil
		}
		return nil, "", fmt.Errorf("mono extraction requires a center or front-left speaker")
	}
	if cfg.Mix == "stereo" || cfg.Mix == "dplii" {
		rows := make([][]contribution, 2)
		for i, role := range roles {
			left, right := 0.0, 0.0
			switch role {
			case "FL":
				left = 1
			case "FR":
				right = 1
			case "FC":
				left, right = math.Sqrt(0.5), math.Sqrt(0.5)
			case "BL", "SL":
				if cfg.Mix == "dplii" {
					left, right = 0.866, -0.5
				} else {
					left = math.Sqrt(0.5)
				}
			case "BR", "SR":
				if cfg.Mix == "dplii" {
					left, right = 0.5, -0.866
				} else {
					right = math.Sqrt(0.5)
				}
			case "BC":
				if cfg.Mix == "dplii" {
					// Round 1.366*sqrt(0.5) once after the extended-precision
					// calculation; double-precision intermediate values differ.
					left, right = 0x1.ee8b79b5ef3fdp-1, -0x1.ee8b79b5ef3fdp-1
				} else {
					left, right = 0.5, 0.5
				}
			case "LFE":
				if cfg.MixLFE {
					left, right = 0.5*math.Sqrt(10), 0.5*math.Sqrt(10)
				}
			default:
				return nil, "", fmt.Errorf("stereo coefficients for speaker %s are not established", role)
			}
			if left != 0 {
				rows[0] = append(rows[0], contribution{i, left})
			}
			if right != 0 {
				rows[1] = append(rows[1], contribution{i, right})
			}
		}
		return rows, "stereo", nil
	}
	return surroundMatrix(cfg.Mix, roles, index)
}

func sameSpeakers(roles []string, expected string) bool {
	set := strings.Split(expected, "+")
	if len(roles) != len(set) {
		return false
	}
	for _, role := range set {
		found := false
		for _, inputRole := range roles {
			found = found || role == inputRole
		}
		if !found {
			return false
		}
	}
	return true
}

func surroundMatrix(mix string, roles []string, index map[string]int) ([][]contribution, string, error) {
	// These routes accept only the canonical 6.1 and 7.1 speaker sets.
	is61 := sameSpeakers(roles, "FL+FR+FC+LFE+BC+SL+SR")
	is71 := sameSpeakers(roles, "FL+FR+FC+LFE+BL+BR+SL+SR")
	identity := func(role string) []contribution { return []contribution{{index[role], 1}} }
	switch mix {
	case "5.1":
		if !is61 && !is71 {
			return nil, "", fmt.Errorf("5.1 downmix requires standard 6.1 or 7.1 speaker positions")
		}
		rows := [][]contribution{identity("FL"), identity("FR"), identity("FC"), identity("LFE"), nil, nil}
		if is71 {
			rows[4] = []contribution{{index["BL"], 1}, {index["SL"], 1}}
			rows[5] = []contribution{{index["BR"], 1}, {index["SR"], 1}}
		} else {
			rows[4] = []contribution{{index["BC"], math.Sqrt(0.5)}, {index["SL"], 1}}
			rows[5] = []contribution{{index["BC"], math.Sqrt(0.5)}, {index["SR"], 1}}
		}
		return rows, "5.1(side)", nil
	case "double7":
		if !is61 {
			return nil, "", fmt.Errorf("7th-channel doubling requires standard 6.1 speaker positions")
		}
		rows := [][]contribution{identity("FL"), identity("FR"), identity("FC"), identity("LFE"),
			{{index["BC"], math.Sqrt(0.5)}}, {{index["BC"], math.Sqrt(0.5)}}, identity("SL"), identity("SR")}
		return rows, "7.1", nil
	default:
		return nil, "", fmt.Errorf("unknown mix operation %q", mix)
	}
}
