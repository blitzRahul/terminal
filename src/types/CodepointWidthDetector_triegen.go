package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

type CharacterWidth int

const (
	cwZeroWidth CharacterWidth = iota
	cwNarrow
	cwWide
	cwAmbiguous
)

type ClusterBreak int

const (
	cbOther ClusterBreak = iota
	cbControl
	cbExtend
	cbPrepend
	cbZeroWidthJoiner
	cbRegionalIndicator
	cbHangulL
	cbHangulV
	cbHangulT
	cbHangulLV
	cbHangulLVT
	cbConjunctLinker
	cbExtendedPictographic

	cbCount
)

type HexInt int

func (h *HexInt) UnmarshalXMLAttr(attr xml.Attr) error {
	v, err := strconv.ParseUint(attr.Value, 16, 32)
	if err != nil {
		return err
	}
	*h = HexInt(v)
	return nil
}

type UCD struct {
	Description string `xml:"description"`
	Repertoire  struct {
		Group []struct {
			GeneralCategory      string `xml:"gc,attr"`
			GraphemeClusterBreak string `xml:"GCB,attr"`
			IndicConjunctBreak   string `xml:"InCB,attr"`
			ExtendedPictographic string `xml:"ExtPict,attr"`
			EastAsian            string `xml:"ea,attr"`

			// This maps the following tags:
			//   <char>, <reserved>, <surrogate>, <noncharacter>
			Char []struct {
				Codepoint      HexInt `xml:"cp,attr"`
				FirstCodepoint HexInt `xml:"first-cp,attr"`
				LastCodepoint  HexInt `xml:"last-cp,attr"`

				GeneralCategory      string `xml:"gc,attr"`
				GraphemeClusterBreak string `xml:"GCB,attr"`
				IndicConjunctBreak   string `xml:"InCB,attr"`
				ExtendedPictographic string `xml:"ExtPict,attr"`
				EastAsian            string `xml:"ea,attr"`
			} `xml:",any"`
		} `xml:"group"`
	} `xml:"repertoire"`
}

func main() {
	if err := run(); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) <= 1 {
		fmt.Println(`Usage:
    go run CodepointWidthDetector_triegen.go <path to ucd.nounihan.grouped.xml>

You can download the latest ucd.nounihan.grouped.xml from:
    https://www.unicode.org/Public/UCD/latest/ucdxml/ucd.nounihan.grouped.zip`)
		os.Exit(1)
	}

	data, err := os.ReadFile(os.Args[1])
	if err != nil {
		return fmt.Errorf("failed to read XML: %w", err)
	}

	ucd := &UCD{}
	err = xml.Unmarshal(data, ucd)
	if err != nil {
		return fmt.Errorf("failed to parse XML: %w", err)
	}

	values, err := extractValuesFromUCD(ucd)
	if err != nil {
		return err
	}

	stages := buildTrie(values, []int{4, 4, 4})
	rules := buildJoinRules()

	for cp, expected := range values {
		var v TrieType
		for _, s := range stages {
			v = s.Values[int(v)+((cp>>s.Shift)&s.Mask)]
		}
		if v != expected {
			return fmt.Errorf("trie sanity check failed for %U", cp)
		}
	}

	totalSize := len(rules) * len(rules)
	for _, s := range stages {
		totalSize += (s.Bits / 8) * len(s.Values)
	}

	buf := &strings.Builder{}

	_, _ = fmt.Fprintf(buf, "// Generated by CodepointWidthDetector_triegen.go\n")
	_, _ = fmt.Fprintf(buf, "// on %v, from %s, %d bytes\n", time.Now().UTC().Format(time.RFC3339), ucd.Description, totalSize)
	_, _ = fmt.Fprintf(buf, "// clang-format off\n")

	for i, s := range stages {
		width := 16
		if i != 0 {
			width = s.Mask + 1
		}
		_, _ = fmt.Fprintf(buf, "static constexpr uint%d_t s_stage%d[] = {", s.Bits, i+1)
		for j, value := range s.Values {
			if j%width == 0 {
				buf.WriteString("\n   ")
			}
			_, _ = fmt.Fprintf(buf, " 0x%0*x,", s.Bits / 4, value)
		}
		buf.WriteString("\n};\n")
	}

	_, _ = fmt.Fprintf(buf, "static constexpr uint8_t s_joinRules[%d][%d] = {", len(rules), len(rules))
	for _, row := range rules {
		buf.WriteString("\n   ")
		for _, val := range row {
			var i int
			if val {
				i = 1
			}
			_, _ = fmt.Fprintf(buf, " %d,", i)
		}
	}
	buf.WriteString("\n};\n")

	buf.WriteString("[[msvc::forceinline]] constexpr uint8_t ucdLookup(const char32_t cp) noexcept\n")
	buf.WriteString("{\n")
	for i, s := range stages {
		_, _ = fmt.Fprintf(buf, "    const auto s%d = s_stage%d[", i+1, i+1)
		if i == 0 {
			_, _ = fmt.Fprintf(buf, "cp >> %d", s.Shift)
		} else {
			_, _ = fmt.Fprintf(buf, "s%d + ((cp >> %d) & %d)", i, s.Shift, s.Mask)
		}
		buf.WriteString("];\n")
	}
	buf.WriteString("}\n")

	buf.WriteString("[[msvc::forceinline]] constexpr uint8_t ucdGraphemeJoins(const uint8_t lead, const uint8_t trail) noexcept\n")
	buf.WriteString("{\n")
	buf.WriteString("    const auto l = lead & 15;\n")
	buf.WriteString("    const auto t = trail & 15;\n")
	buf.WriteString("    return s_joinRules[l][t];\n")
	buf.WriteString("}\n")

	buf.WriteString("[[msvc::forceinline]] constexpr int ucdToCharacterWidth(const uint8_t val) noexcept\n")
	buf.WriteString("{\n")
	buf.WriteString("    return val >> 6;\n")
	buf.WriteString("}\n")

	buf.WriteString("// clang-format on\n")

	_, _ = os.Stdout.WriteString(buf.String())
	return nil
}

type TrieType uint32

func extractValuesFromUCD(ucd *UCD) ([]TrieType, error) {
	values := make([]TrieType, 1114112)
	fillRange(values, trieValue(cbOther, cwNarrow))

	for _, group := range ucd.Repertoire.Group {
		for _, char := range group.Char {
			generalCategory := coalesce(char.GeneralCategory, group.GeneralCategory)
			graphemeClusterBreak := coalesce(char.GraphemeClusterBreak, group.GraphemeClusterBreak)
			indicConjunctBreak := coalesce(char.IndicConjunctBreak, group.IndicConjunctBreak)
			extendedPictographic := coalesce(char.ExtendedPictographic, group.ExtendedPictographic)
			eastAsian := coalesce(char.EastAsian, group.EastAsian)

			firstCp, lastCp := int(char.FirstCodepoint), int(char.LastCodepoint)
			if char.Codepoint != 0 {
				firstCp, lastCp = int(char.Codepoint), int(char.Codepoint)
			}

			var (
				cb    ClusterBreak
				width CharacterWidth
			)

			switch graphemeClusterBreak {
			case "XX": // Anything else
				cb = cbOther
			case "CR", "LF", "CN": // Carriage Return, Line Feed, Control
				// We ignore GB3 which demands that CR × LF do not break apart, because
				// a) these control characters won't normally reach our text storage
				// b) otherwise we're in a raw write mode and historically conhost stores them in separate cells
				cb = cbControl
			case "EX", "SM": // Extend, SpacingMark
				cb = cbExtend
			case "PP": // Prepend
				cb = cbPrepend
			case "ZWJ": // Zero Width Joiner
				cb = cbZeroWidthJoiner
			case "RI": // Regional Indicator
				cb = cbRegionalIndicator
			case "L": // Hangul Syllable Type L
				cb = cbHangulL
			case "V": // Hangul Syllable Type V
				cb = cbHangulV
			case "T": // Hangul Syllable Type T
				cb = cbHangulT
			case "LV": // Hangul Syllable Type LV
				cb = cbHangulLV
			case "LVT": // Hangul Syllable Type LVT
				cb = cbHangulLVT
			default:
				return nil, fmt.Errorf("unrecognized GCB %s for %U to %U", graphemeClusterBreak, firstCp, lastCp)
			}

			if extendedPictographic == "Y" {
				// Currently every single Extended_Pictographic codepoint happens to be GCB=XX.
				// This is fantastic for us because it means we can stuff it into the ClusterBreak enum
				// and treat it as an alias of EXTEND, but with the special GB11 properties.
				if cb != cbOther {
					return nil, fmt.Errorf("unexpected GCB %s for ExtPict for %U to %U", graphemeClusterBreak, firstCp, lastCp)
				}
				cb = cbExtendedPictographic
			}

			if indicConjunctBreak == "Linker" {
				// Similarly here, we can treat it as an alias for EXTEND, but with the GB9c properties.
				if cb != cbExtend {
					return nil, fmt.Errorf("unexpected GCB %s for InCB=Linker for %U to %U", graphemeClusterBreak, firstCp, lastCp)
				}
				cb = cbConjunctLinker
			}

			switch eastAsian {
			case "N", "Na", "H": // neutral, narrow, half-width
				width = cwNarrow
			case "F", "W": // full-width, wide
				width = cwWide
			case "A": // ambiguous
				width = cwAmbiguous
			default:
				return nil, fmt.Errorf("unrecognized ea %s for %U to %U", eastAsian, firstCp, lastCp)
			}

			// There's no "ea" attribute for "zero width" so we need to do that ourselves. This matches:
			//   Mc: Mark, spacing combining
			//   Me: Mark, enclosing
			//   Mn: Mark, non-spacing
			//   Cf: Control, format
			if strings.HasPrefix(generalCategory, "M") || generalCategory == "Cf" {
				width = cwZeroWidth
			}

			fillRange(values[firstCp:lastCp+1], trieValue(cb, width))
		}
	}

	// Box-drawing and block elements require 1-cell alignment.
	// Most characters in this range have an ambiguous width otherwise.
	fillRange(values[0x2500:0x259F+1], trieValue(cbOther, cwNarrow))
	// hexagrams are historically narrow
	fillRange(values[0x4DC0:0x4DFF+1], trieValue(cbOther, cwNarrow))
	// narrow combining ligatures (split into left/right halves, which take 2 columns together)
	fillRange(values[0xFE20:0xFE2F+1], trieValue(cbOther, cwNarrow))

	return values, nil
}

func trieValue(cb ClusterBreak, width CharacterWidth) TrieType {
	return TrieType(byte(cb) | byte(width)<<6)
}

func coalesce(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

type Stage struct {
	Values []TrieType
	Shift  int
	Mask   int
	Bits   int
}

func buildTrie(uncompressed []TrieType, shifts []int) []*Stage {
	var cumulativeShift int
	var stages []*Stage

	for _, shift := range shifts {
		chunkSize := 1 << shift
		cache := map[string]TrieType{}
		compressed := make([]TrieType, 0, len(uncompressed)/8)
		offsets := make([]TrieType, 0, len(uncompressed)/chunkSize)

		for i := 0; i < len(uncompressed); i += chunkSize {
			chunk := uncompressed[i : i+chunkSize]
			// Cast the integer slice to a string so that it can be hashed.
			key := unsafe.String((*byte)(unsafe.Pointer(&chunk[0])), len(chunk)*int(unsafe.Sizeof(chunk[0])))
			offset, exists := cache[key]

			if !exists {
				// For a 4-stage trie searching for existing occurrences of chunk in compressed yields a ~10%
				// compression improvement. Checking for overlaps with the tail end of compressed yields another ~15%.
				// FYI I tried to shuffle the order of compressed chunks but found that this has a negligible impact.
				if existing := findExisting(compressed, chunk); existing != -1 {
					offset = TrieType(existing)
					cache[key] = offset
				} else {
					overlap := measureOverlap(compressed, chunk)
					compressed = append(compressed, chunk[overlap:]...)
					offset = TrieType(len(compressed) - len(chunk))
					cache[key] = offset
				}
			}

			offsets = append(offsets, offset)
		}

		stages = append(stages, &Stage{
			Values: compressed,
			Shift:  cumulativeShift,
			Mask:   chunkSize - 1,
		})

		uncompressed = offsets
		cumulativeShift += shift
	}

	stages = append(stages, &Stage{
		Values: uncompressed,
		Shift:  cumulativeShift,
		Mask:   math.MaxInt32,
	})

	for _, s := range stages {
		m := slices.Max(s.Values)
		if m <= 0xff {
			s.Bits = 8
		} else if m <= 0xffff {
			s.Bits = 16
		} else {
			s.Bits = 32
		}
	}

	slices.Reverse(stages)
	return stages
}

// Finds needle in haystack. Returns -1 if it couldn't be found.
func findExisting(haystack, needle []TrieType) int {
	if len(haystack) == 0 || len(needle) == 0 {
		return -1
	}

	s := int(unsafe.Sizeof(TrieType(0)))
	h := unsafe.Slice((*byte)(unsafe.Pointer(&haystack[0])), len(haystack)*s)
	n := unsafe.Slice((*byte)(unsafe.Pointer(&needle[0])), len(needle)*s)
	i := 0

	for {
		i = bytes.Index(h[i:], n)
		if i == -1 {
			return -1
		}
		if i%s == 0 {
			return i / s
		}
	}
}

// Given two slices, this returns the amount by which prev's end overlaps with next's start.
// That is, given [0,1,2,3,4] and [2,3,4,5] this returns 3 because [2,3,4] is the "overlap".
func measureOverlap(prev, next []TrieType) int {
	for overlap := min(len(prev), len(next)); overlap >= 0; overlap-- {
		if slices.Equal(prev[len(prev)-overlap:], next[:overlap]) {
			return overlap
		}
	}
	return 0
}

func buildJoinRules() [cbCount][cbCount]bool {
	// UAX #29 states:
	// > Note: Testing two adjacent characters is insufficient for determining a boundary.
	//
	// I completely agree, but I really hate it. So this code trades off correctness for simplicity
	// by using a simple lookup table anyway. Under most circumstances users won't notice,
	// because as far as I can see this only behaves different for degenerate ("invalid") Unicode.
	// It reduces our code complexity significantly and is way *way* faster.
	//
	// This is a great reference for the resulting table:
	//   https://www.unicode.org/Public/UCD/latest/ucd/auxiliary/GraphemeBreakTest.html

	// NOTE: We build the table in reverse, because rules with lower numbers take priority.
	// (This is primarily relevant for GB9b vs. GB4.)

	// Otherwise, break everywhere.
	// GB999: Any ÷ Any
	var rules [cbCount][cbCount]bool

	// Do not break within emoji flag sequences. That is, do not break between regional indicator
	// (RI) symbols if there is an odd number of RI characters before the break point.
	// GB13: [^RI] (RI RI)* RI × RI
	// GB12: sot (RI RI)* RI × RI
	//
	// We cheat here by not checking that the number of RIs is even. Meh!
	rules[cbRegionalIndicator][cbRegionalIndicator] = true

	// Do not break within emoji modifier sequences or emoji zwj sequences.
	// GB11: \p{Extended_Pictographic} Extend* ZWJ × \p{Extended_Pictographic}
	//
	// We cheat here by not checking that the ZWJ is preceded by an ExtPic. Meh!
	rules[cbZeroWidthJoiner][cbExtendedPictographic] = true

	// Do not break within certain combinations with Indic_Conjunct_Break (InCB)=Linker.
	// GB9c: \p{InCB=Consonant} [\p{InCB=Extend}\p{InCB=Linker}]* \p{InCB=Linker} [\p{InCB=Extend}\p{InCB=Linker}]* × \p{InCB=Consonant}
	//
	// I'm sure GB9c is great for these languages, but honestly the definition is complete whack.
	// Just look at that chonker! This isn't a "cheat" like the others above, this is a reinvention:
	// We treat it as having both ClusterBreak.PREPEND and ClusterBreak.EXTEND properties.
	fillRange(rules[cbConjunctLinker][:], true)
	for i := range rules {
		rules[i][cbConjunctLinker] = true
	}

	// Do not break before SpacingMarks, or after Prepend characters.
	// GB9b: Prepend ×
	fillRange(rules[cbPrepend][:], true)

	// Do not break before SpacingMarks, or after Prepend characters.
	// GB9a: × SpacingMark
	// Do not break before extending characters or ZWJ.
	// GB9: × (Extend | ZWJ)
	for i := range rules {
		// CodepointWidthDetector_triegen.py sets SpacingMarks to ClusterBreak.EXTEND as well,
		// since they're entirely identical to GB9's Extend.
		rules[i][cbExtend] = true
		rules[i][cbZeroWidthJoiner] = true
	}

	// Do not break Hangul syllable sequences.
	// GB8: (LVT | T) x T
	rules[cbHangulLVT][cbHangulT] = true
	rules[cbHangulT][cbHangulT] = true
	// GB7: (LV | V) x (V | T)
	rules[cbHangulLV][cbHangulT] = true
	rules[cbHangulLV][cbHangulV] = true
	rules[cbHangulV][cbHangulV] = true
	rules[cbHangulV][cbHangulT] = true
	// GB6: L x (L | V | LV | LVT)
	rules[cbHangulL][cbHangulL] = true
	rules[cbHangulL][cbHangulV] = true
	rules[cbHangulL][cbHangulLV] = true
	rules[cbHangulL][cbHangulLVT] = true

	// Do not break between a CR and LF. Otherwise, break before and after controls.
	// GB5: ÷ (Control | CR | LF)
	for i := range rules {
		rules[i][cbControl] = false
	}
	// GB4: (Control | CR | LF) ÷
	fillRange(rules[cbControl][:], false)

	// We ignore GB3 which demands that CR × LF do not break apart, because
	// a) these control characters won't normally reach our text storage
	// b) otherwise we're in a raw write mode and historically conhost stores them in separate cells

	// We also ignore GB1 and GB2 which demand breaks at the start and end,
	// because that's not part of the loops in GraphemeNext/Prev and not this table.
	return rules
}

func fillRange[T any](s []T, v T) {
	for i := range s {
		s[i] = v
	}
}