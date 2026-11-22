package receipts

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/meters"
	"github.com/teamthy/meterly/internal/money"
	tx "github.com/teamthy/meterly/internal/transactions"
)

func TestBuildIsMinimalAndStatusHonest(t *testing.T) {
	id := uuid.New()
	meter := &meters.Meter{ID: id, Masked: "*******5678", DiscoCode: "TEST"}
	tnx := &tx.Transaction{ID: uuid.New(), MeterID: id, Amount: money.New(100, money.NGN), Fee: money.New(5, money.NGN), Total: money.New(105, money.NGN), Payment: tx.PayConfirmed, Vending: tx.VendConfirmed, Delivery: tx.DelAccepted, DeliveryMethod: tx.MethodRemote, TokenCiphertext: []byte("SYNTHETIC_SECRET"), CheckoutURL: "PRIVATE_CHECKOUT", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	r, err := Build(tnx, meter)
	if err != nil {
		t.Fatal(err)
	}
	if r.OverallStatus != tx.OverallDeliveryPending || r.DeliveryStatus != tx.DelAccepted {
		t.Fatal("accepted was treated as delivery")
	}
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "SYNTHETIC_SECRET") || strings.Contains(string(b), "PRIVATE_CHECKOUT") {
		t.Fatal("sensitive data leaked")
	}
	if _, err := Build(tnx, &meters.Meter{ID: uuid.New(), Masked: "****"}); err == nil {
		t.Fatal("mismatched meter accepted")
	}
}
