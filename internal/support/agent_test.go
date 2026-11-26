package support

import "testing"

func TestAnswerNeverEchoesAToken(t *testing.T) {
	token := "1234 5678 9012 3456 7890"
	r := Answer("my token is "+token, nil)
	if r.MatchedIntent != "token_paste" {
		t.Fatalf("intent=%s body=%s", r.MatchedIntent, r.Body)
	}
	if containsAny(r.Body, "1234", "5678", "9012") {
		t.Fatalf("agent echoed token digits: %s", r.Body)
	}
}

func TestAnswerLooksUpOwnTransaction(t *testing.T) {
	id := "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	r := Answer("what is the status of "+id+"?", []TxnSummary{{
		ID: id, Status: "COMPLETED", Payment: "PAID", Vending: "VENDED",
		Delivery: "CONFIRMED", DeliveryMethod: "TOKEN", AmountMinor: 500000, CreatedAt: "2026-01-02T00:00:00Z",
	}})
	if r.MatchedIntent != "txn_lookup" {
		t.Fatalf("intent=%s", r.MatchedIntent)
	}
	if !containsAny(r.Body, "PAID", "VENDED", "CONFIRMED") {
		t.Fatalf("missing status in %s", r.Body)
	}
}

func TestAnswerSuggestsHuman(t *testing.T) {
	r := Answer("I want to talk to a real person please", nil)
	if !r.SuggestHuman || r.MatchedIntent != "human" {
		t.Fatalf("%+v", r)
	}
}

func TestWelcomeDoesNotClaimDelivery(t *testing.T) {
	w := Welcome()
	if containsAny(w, "delivered to your meter", "credit is in the meter") {
		t.Fatalf("welcome overclaims delivery: %s", w)
	}
}
