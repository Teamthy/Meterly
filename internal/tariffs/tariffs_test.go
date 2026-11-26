package tariffs

import "testing"

func TestKoboPerKWhByDiscoAndBand(t *testing.T) {
	if got := KoboPerKWh("IKEDC", BandA); got != 20950 {
		t.Fatalf("IKEDC A: %d", got)
	}
	if got := KoboPerKWh("ikedc", ""); got != 20950 {
		t.Fatalf("default band: %d", got)
	}
	if got := KoboPerKWh("EKEDC", BandA); got == KoboPerKWh("IKEDC", BandA) {
		t.Fatal("EKEDC and IKEDC Band A should differ")
	}
	if got := KoboPerKWh("IKEDC", BandE); got >= KoboPerKWh("IKEDC", BandA) {
		t.Fatalf("Band E should be cheaper than A: %d", got)
	}
	if got := KoboPerKWh("UNKNOWN", BandA); got != 20950 {
		t.Fatalf("unknown disco falls back to IKEDC A: %d", got)
	}
}

func TestUnitsFromIntegerTariff(t *testing.T) {
	// ₦480 at ₦209.50/kWh = 48000/20950 ≈ 2.291 kWh
	u := UnitsKWh(48_000, 20_950)
	if u < 2.29 || u > 2.292 {
		t.Fatalf("units %v", u)
	}
	if UnitsKWh(0, 20_950) != 0 || UnitsKWh(50_000, 0) != 0 {
		t.Fatal("zero inputs")
	}
}

func TestRecommendSnapsToPack(t *testing.T) {
	if p := Recommend(0); p.ID != DefaultPackID {
		t.Fatalf("plenty left: %s", p.ID)
	}
	if p := Recommend(10_000); p.ID != "ngn-100" {
		t.Fatalf("₦100 cover: %s", p.ID)
	}
	if p := Recommend(48_000); p.ID != "ngn-500" {
		t.Fatalf("₦480 cover: %s", p.ID)
	}
	if p := Recommend(200_000); p.ID != "ngn-2000" {
		t.Fatalf("over largest: %s", p.ID)
	}
}

func TestPackDiscount(t *testing.T) {
	p, ok := PackByID("ngn-500")
	if !ok || DiscountMinor(p) != 2_000 || p.PayMinor != 48_000 {
		t.Fatalf("%+v ok=%v", p, ok)
	}
	p, _ = PackByID("ngn-100")
	if DiscountMinor(p) != 0 {
		t.Fatalf("no discount: %+v", p)
	}
}
