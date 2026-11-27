package disputes_test

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/teamthy/meterly/internal/disputes"
	"github.com/teamthy/meterly/internal/ledger"
	"github.com/teamthy/meterly/internal/money"
)

func TestDisputePostingsBalance(t *testing.T) {
	d := disputes.Dispute{
		ID: uuid.New(), PSPDisputeID: "2867",
		Amount: money.New(50_000, money.NGN),
	}
	now := time.Now().UTC()
	for _, txn := range []ledger.Transaction{
		disputes.OpenPosting(d, now),
		disputes.WinPosting(d, now),
		disputes.LossPosting(d, now),
	} {
		if err := ledger.Validate(txn.Entries); err != nil {
			t.Fatalf("%s: %v", txn.Reference, err)
		}
	}
	if disputes.OpenPosting(d, now).Reference != "dispute-open:2867" {
		t.Fatal("open ref")
	}
	if disputes.OutcomeFromResolution("merchant-accepted", "resolved") != disputes.StatusAccepted {
		t.Fatal("accepted")
	}
	if disputes.OutcomeFromResolution("declined", "resolved") != disputes.StatusWon {
		t.Fatal("won")
	}
}
