package payrequests

import (
	"bytes"
	"encoding/base64"
	"errors"
	"image"
	"image/color"
	"image/png"
)

// qrSpec holds single-block ISO/IEC 18004 Level-L parameters for Versions 1–5.
type qrSpec struct {
	version    int
	size       int
	dataBytes  int // total data codewords
	ecBytes    int // Reed-Solomon ECC codewords (single block)
	alignCoord []int
}

var qrSpecsL = []qrSpec{
	{version: 1, size: 21, dataBytes: 19, ecBytes: 7, alignCoord: nil},
	{version: 2, size: 25, dataBytes: 34, ecBytes: 10, alignCoord: []int{6, 18}},
	{version: 3, size: 29, dataBytes: 55, ecBytes: 15, alignCoord: []int{6, 22}},
	{version: 4, size: 33, dataBytes: 80, ecBytes: 20, alignCoord: []int{6, 26}},
	{version: 5, size: 37, dataBytes: 108, ecBytes: 26, alignCoord: []int{6, 30}},
}

// EncodeQRPNGBase64 encodes text as a scannable ISO/IEC 18004 QR Code PNG and
// returns its standard base64 representation.
func EncodeQRPNGBase64(text string) (string, error) {
	pngBytes, err := EncodeQRPNG(text, 6)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(pngBytes), nil
}

// EncodeQRPNG encodes text (up to 106 bytes, Byte mode, ECC Level L) into a
// monochrome PNG with a 4-module quiet zone.
func EncodeQRPNG(text string, scale int) ([]byte, error) {
	raw := []byte(text)
	if len(raw) == 0 {
		return nil, errors.New("payrequests: qr payload cannot be empty")
	}
	if scale < 1 {
		scale = 6
	}

	var spec qrSpec
	for _, s := range qrSpecsL {
		// Byte mode overhead on V1–V9: 4-bit mode (0100) + 8-bit length = 12 bits -> max dataBytes-2 payload bytes.
		if len(raw) <= s.dataBytes-2 {
			spec = s
			break
		}
	}
	if spec.version == 0 {
		return nil, errors.New("payrequests: qr payload exceeds version 5 capacity")
	}

	dataCW := buildByteModeCodewords(raw, spec.dataBytes)
	eccCW := reedSolomonEncode(dataCW, spec.ecBytes)
	allCW := append(dataCW, eccCW...)

	matrix := buildQRMatrix(spec, allCW)

	const quiet = 4
	totalModules := spec.size + 2*quiet
	pxSize := totalModules * scale
	img := image.NewGray(image.Rect(0, 0, pxSize, pxSize))
	for i := range img.Pix {
		img.Pix[i] = 255 // white background
	}
	for r := 0; r < spec.size; r++ {
		for c := 0; c < spec.size; c++ {
			if !matrix[r][c] {
				continue
			}
			y0 := (r + quiet) * scale
			x0 := (c + quiet) * scale
			for dy := 0; dy < scale; dy++ {
				for dx := 0; dx < scale; dx++ {
					img.SetGray(x0+dx, y0+dy, color.Gray{Y: 0})
				}
			}
		}
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildByteModeCodewords(payload []byte, totalDataCW int) []byte {
	var bits []uint8
	appendBits := func(val int, n int) {
		for i := n - 1; i >= 0; i-- {
			bits = append(bits, uint8((val>>i)&1))
		}
	}
	// Mode indicator for Byte mode = 0100.
	appendBits(0b0100, 4)
	// Character count indicator (8 bits for Versions 1–9).
	appendBits(len(payload), 8)
	for _, b := range payload {
		appendBits(int(b), 8)
	}
	capacityBits := totalDataCW * 8
	for i := 0; i < 4 && len(bits) < capacityBits; i++ {
		bits = append(bits, 0)
	}
	for len(bits)%8 != 0 {
		bits = append(bits, 0)
	}
	out := make([]byte, 0, totalDataCW)
	for i := 0; i < len(bits); i += 8 {
		var cw byte
		for j := 0; j < 8; j++ {
			cw = (cw << 1) | bits[i+j]
		}
		out = append(out, cw)
	}
	pads := [2]byte{0xEC, 0x11}
	for idx := 0; len(out) < totalDataCW; idx++ {
		out = append(out, pads[idx%2])
	}
	return out
}

// GF(256) arithmetic with primitive polynomial x^8 + x^4 + x^3 + x^2 + 1 (0x11D).
var (
	gfExp [512]byte
	gfLog [256]byte
)

func init() {
	x := 1
	for i := 0; i < 255; i++ {
		gfExp[i] = byte(x)
		gfLog[x] = byte(i)
		x <<= 1
		if x&0x100 != 0 {
			x ^= 0x11D
		}
	}
	for i := 255; i < 512; i++ {
		gfExp[i] = gfExp[i-255]
	}
}

func gfMul(a, b byte) byte {
	if a == 0 || b == 0 {
		return 0
	}
	return gfExp[int(gfLog[a])+int(gfLog[b])]
}

func reedSolomonGenerator(degree int) []byte {
	g := []byte{1}
	for i := 0; i < degree; i++ {
		next := make([]byte, len(g)+1)
		factor := gfExp[i]
		for j, coef := range g {
			next[j] ^= coef
			next[j+1] ^= gfMul(coef, factor)
		}
		g = next
	}
	return g
}

func reedSolomonEncode(data []byte, ecLen int) []byte {
	gen := reedSolomonGenerator(ecLen)
	rem := make([]byte, ecLen)
	for _, b := range data {
		factor := b ^ rem[0]
		copy(rem, rem[1:])
		rem[ecLen-1] = 0
		for j := 0; j < ecLen; j++ {
			rem[j] ^= gfMul(gen[j+1], factor)
		}
	}
	return rem
}

func buildQRMatrix(spec qrSpec, codewords []byte) [][]bool {
	n := spec.size
	modules := make([][]bool, n)
	reserved := make([][]bool, n)
	for i := 0; i < n; i++ {
		modules[i] = make([]bool, n)
		reserved[i] = make([]bool, n)
	}

	setFn := func(r, c int, dark bool) {
		if r >= 0 && r < n && c >= 0 && c < n {
			modules[r][c] = dark
			reserved[r][c] = true
		}
	}

	placeFinder := func(topR, leftC int) {
		for dr := -1; dr <= 7; dr++ {
			for dc := -1; dc <= 7; dc++ {
				r, c := topR+dr, leftC+dc
				if r < 0 || r >= n || c < 0 || c >= n {
					continue
				}
				inOuter := dr >= 0 && dr <= 6 && dc >= 0 && dc <= 6
				onBorder := dr == 0 || dr == 6 || dc == 0 || dc == 6
				inCenter := dr >= 2 && dr <= 4 && dc >= 2 && dc <= 4
				setFn(r, c, inOuter && (onBorder || inCenter))
			}
		}
	}

	placeFinder(0, 0)
	placeFinder(0, n-7)
	placeFinder(n-7, 0)

	// Timing patterns.
	for i := 8; i < n-8; i++ {
		if !reserved[6][i] {
			setFn(6, i, i%2 == 0)
		}
		if !reserved[i][6] {
			setFn(i, 6, i%2 == 0)
		}
	}

	// Alignment patterns (Versions >= 2).
	for _, ar := range spec.alignCoord {
		for _, ac := range spec.alignCoord {
			if reserved[ar][ac] {
				continue
			}
			for dr := -2; dr <= 2; dr++ {
				for dc := -2; dc <= 2; dc++ {
					dark := dr == -2 || dr == 2 || dc == -2 || dc == 2 || (dr == 0 && dc == 0)
					setFn(ar+dr, ac+dc, dark)
				}
			}
		}
	}

	// Dark module + reserve format info strips.
	setFn(4*spec.version+9, 8, true)
	for i := 0; i < 9; i++ {
		if !reserved[8][i] {
			reserved[8][i] = true
		}
		if !reserved[i][8] {
			reserved[i][8] = true
		}
	}
	for i := 0; i < 8; i++ {
		reserved[8][n-1-i] = true
		reserved[n-1-i][8] = true
	}

	// Place data + ECC bits upward/downward in 2-column strips.
	bitIdx := 0
	totalBits := len(codewords) * 8
	upward := true
	for right := n - 1; right >= 1; right -= 2 {
		if right == 6 {
			right = 5 // skip vertical timing column
		}
		for step := 0; step < n; step++ {
			r := step
			if upward {
				r = n - 1 - step
			}
			for dc := 0; dc < 2; dc++ {
				c := right - dc
				if reserved[r][c] {
					continue
				}
				dark := false
				if bitIdx < totalBits {
					dark = ((codewords[bitIdx/8] >> (7 - (bitIdx % 8))) & 1) == 1
					bitIdx++
				}
				modules[r][c] = dark
			}
		}
		upward = !upward
	}

	// Pick lowest-penalty mask among masks 0..7.
	bestMask := 0
	bestPenalty := int(^uint(0) >> 1)
	var bestGrid [][]bool
	for mask := 0; mask < 8; mask++ {
		cand := cloneGrid(modules)
		applyMask(cand, reserved, mask)
		writeFormatBits(cand, mask)
		if p := maskPenalty(cand); p < bestPenalty {
			bestPenalty = p
			bestMask = mask
			bestGrid = cand
		}
	}
	_ = bestMask
	return bestGrid
}

func cloneGrid(src [][]bool) [][]bool {
	out := make([][]bool, len(src))
	for i := range src {
		out[i] = append([]bool(nil), src[i]...)
	}
	return out
}

func applyMask(grid, reserved [][]bool, mask int) {
	n := len(grid)
	for r := 0; r < n; r++ {
		for c := 0; c < n; c++ {
			if reserved[r][c] {
				continue
			}
			var flip bool
			switch mask {
			case 0:
				flip = (r+c)%2 == 0
			case 1:
				flip = r%2 == 0
			case 2:
				flip = c%3 == 0
			case 3:
				flip = (r+c)%3 == 0
			case 4:
				flip = (r/2+c/3)%2 == 0
			case 5:
				flip = (r*c)%2+(r*c)%3 == 0
			case 6:
				flip = ((r*c)%2+(r*c)%3)%2 == 0
			case 7:
				flip = ((r+c)%2+(r*c)%3)%2 == 0
			}
			if flip {
				grid[r][c] = !grid[r][c]
			}
		}
	}
}

// writeFormatBits writes BCH(15,5) format bits for ECC Level L (01) and mask 0..7.
func writeFormatBits(grid [][]bool, mask int) {
	// ECC Level L indicator = 0b01.
	data := (0b01 << 3) | (mask & 0b111)
	rem := data << 10
	for i := 14; i >= 10; i-- {
		if (rem>>i)&1 != 0 {
			rem ^= 0x537 << (i - 10)
		}
	}
	bits := ((data << 10) | rem) ^ 0x5412

	n := len(grid)
	bit := func(i int) bool { return ((bits >> i) & 1) == 1 }

	// Top-left around finder.
	for i := 0; i <= 5; i++ {
		grid[8][i] = bit(i)
	}
	grid[8][7] = bit(6)
	grid[8][8] = bit(7)
	grid[7][8] = bit(8)
	for i := 9; i < 15; i++ {
		grid[14-i][8] = bit(i)
	}

	// Top-right & bottom-left copies.
	for i := 0; i < 8; i++ {
		grid[8][n-1-i] = bit(i)
	}
	for i := 8; i < 15; i++ {
		grid[n-15+i][8] = bit(i)
	}
}

func maskPenalty(grid [][]bool) int {
	n := len(grid)
	penalty := 0
	for r := 0; r < n; r++ {
		runLen := 1
		for c := 1; c < n; c++ {
			if grid[r][c] == grid[r][c-1] {
				runLen++
				if runLen == 5 {
					penalty += 3
				} else if runLen > 5 {
					penalty++
				}
			} else {
				runLen = 1
			}
		}
	}
	for c := 0; c < n; c++ {
		runLen := 1
		for r := 1; r < n; r++ {
			if grid[r][c] == grid[r-1][c] {
				runLen++
				if runLen == 5 {
					penalty += 3
				} else if runLen > 5 {
					penalty++
				}
			} else {
				runLen = 1
			}
		}
	}
	for r := 0; r < n-1; r++ {
		for c := 0; c < n-1; c++ {
			v := grid[r][c]
			if grid[r][c+1] == v && grid[r+1][c] == v && grid[r+1][c+1] == v {
				penalty += 3
			}
		}
	}
	return penalty
}
