// Package tariffs is the server-side catalogue of Nigerian DISCOs, MYTO-style
// band rates (kobo per kWh) and prepaid amount packs. The web app must display
// these values, not recompute units or pay amounts.
package tariffs

import (
	"math"
	"strings"
)

type Band string

const (
	BandA       Band = "A"
	BandB       Band = "B"
	BandC       Band = "C"
	BandD       Band = "D"
	BandE       Band = "E"
	DefaultBand Band = BandA
)

// Disco is a distribution company the purchaser can pick.
type Disco struct {
	Code string `json:"code"`
	Name string `json:"name"`
}

// Pack is a prepaid amount tile. CreditMinor is the face value shown on the
// tile; PayMinor is what the customer is charged (and what Meterly vends).
type Pack struct {
	ID          string `json:"id"`
	CreditMinor int64  `json:"credit_minor"`
	PayMinor    int64  `json:"pay_minor"`
}

var discos = []Disco{
	{Code: "IKEDC", Name: "Ikeja Electric (IKEDC)"},
	{Code: "EKEDC", Name: "Eko Electricity (EKEDC)"},
	{Code: "AEDC", Name: "Abuja Electricity (AEDC)"},
	{Code: "IBEDC", Name: "Ibadan Electricity (IBEDC)"},
	{Code: "EEDC", Name: "Enugu Electricity (EEDC)"},
	{Code: "PHED", Name: "Port Harcourt Electric (PHED)"},
	{Code: "KEDCO", Name: "Kano Electricity (KEDCO)"},
	{Code: "KAEDCO", Name: "Kaduna Electric (KAEDCO)"},
	{Code: "JED", Name: "Jos Electricity (JED)"},
	{Code: "YEDC", Name: "Yola Electricity (YEDC)"},
}

// koboPerKWh is integer kobo per kilowatt-hour, per DISCO and service band.
// IKEDC Band A is 20950 (₦209.50) so the mock vending display of 209.5 matches.
var koboPerKWh = map[string]map[Band]int64{
	"IKEDC":  {BandA: 20950, BandB: 17120, BandC: 13780, BandD: 9970, BandE: 6840},
	"EKEDC":  {BandA: 20720, BandB: 16940, BandC: 13620, BandD: 9860, BandE: 6760},
	"AEDC":   {BandA: 21200, BandB: 17340, BandC: 13950, BandD: 10100, BandE: 6930},
	"IBEDC":  {BandA: 20580, BandB: 16820, BandC: 13540, BandD: 9800, BandE: 6720},
	"EEDC":   {BandA: 19840, BandB: 16220, BandC: 13050, BandD: 9440, BandE: 6480},
	"PHED":   {BandA: 21060, BandB: 17220, BandC: 13860, BandD: 10030, BandE: 6880},
	"KEDCO":  {BandA: 18650, BandB: 15250, BandC: 12270, BandD: 8880, BandE: 6100},
	"KAEDCO": {BandA: 18420, BandB: 15060, BandC: 12120, BandD: 8770, BandE: 6020},
	"JED":    {BandA: 19110, BandB: 15620, BandC: 12570, BandD: 9100, BandE: 6240},
	"YEDC":   {BandA: 17980, BandB: 14700, BandC: 11830, BandD: 8560, BandE: 5880},
}

var packs = []Pack{
	{ID: "ngn-100", CreditMinor: 10_000, PayMinor: 10_000},
	{ID: "ngn-200", CreditMinor: 20_000, PayMinor: 20_000},
	{ID: "ngn-300", CreditMinor: 30_000, PayMinor: 30_000},
	{ID: "ngn-500", CreditMinor: 50_000, PayMinor: 48_000},
	{ID: "ngn-1000", CreditMinor: 100_000, PayMinor: 97_000},
	{ID: "ngn-2000", CreditMinor: 200_000, PayMinor: 195_000},
}

const DefaultPackID = "ngn-1000"

func Discos() []Disco {
	out := make([]Disco, len(discos))
	copy(out, discos)
	return out
}

func DiscoName(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	for _, d := range discos {
		if d.Code == c {
			return d.Name
		}
	}
	if c == "" {
		return "Unknown DISCO"
	}
	return c
}

func NormalizeBand(band string) Band {
	switch Band(strings.ToUpper(strings.TrimSpace(band))) {
	case BandA, BandB, BandC, BandD, BandE:
		return Band(strings.ToUpper(strings.TrimSpace(band)))
	default:
		return DefaultBand
	}
}

func KoboPerKWh(disco string, band Band) int64 {
	c := strings.ToUpper(strings.TrimSpace(disco))
	b := band
	if b == "" {
		b = DefaultBand
	}
	if byBand, ok := koboPerKWh[c]; ok {
		if v, ok := byBand[b]; ok && v > 0 {
			return v
		}
	}
	return koboPerKWh["IKEDC"][DefaultBand]
}

func NairaPerKWh(kobo int64) float64 {
	return math.Round(float64(kobo)) / 100
}

// UnitsKWh is kWh that will be served for amountMinor kobo at koboPerKWh.
func UnitsKWh(amountMinor, koboPerKWh int64) float64 {
	if koboPerKWh <= 0 || amountMinor <= 0 {
		return 0
	}
	return math.Round(float64(amountMinor)/float64(koboPerKWh)*1000) / 1000
}

func Packs() []Pack {
	out := make([]Pack, len(packs))
	copy(out, packs)
	return out
}

func PackByID(id string) (Pack, bool) {
	id = strings.TrimSpace(id)
	for _, p := range packs {
		if p.ID == id {
			return p, true
		}
	}
	return Pack{}, false
}

func DiscountMinor(p Pack) int64 {
	d := p.CreditMinor - p.PayMinor
	if d < 0 {
		return 0
	}
	return d
}

// Recommend returns the smallest pack whose PayMinor covers neededMinor.
// When neededMinor <= 0 (plenty of credit), it returns the default ₦1,000 pack.
func Recommend(neededMinor int64) Pack {
	if neededMinor <= 0 {
		p, _ := PackByID(DefaultPackID)
		return p
	}
	var best Pack
	for _, p := range packs {
		if p.PayMinor >= neededMinor && (best.ID == "" || p.PayMinor < best.PayMinor) {
			best = p
		}
	}
	if best.ID == "" {
		return packs[len(packs)-1]
	}
	return best
}
