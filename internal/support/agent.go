package support

import (
	"fmt"
	"strings"
	"unicode"
)

// TxnSummary is a redacted view of the caller's own purchase. Tokens,
// meter numbers, checkout URLs and provider references must never appear.
type TxnSummary struct {
	ID             string
	Status         string
	Payment        string
	Vending        string
	Delivery       string
	DeliveryMethod string
	AmountMinor    int64
	Currency       string
	IsGift         bool
	CreatedAt      string
}

type Reply struct {
	Body          string
	SuggestHuman  bool
	MatchedIntent string
}

const welcome = `Welcome to Meterly Support. I'm the Meterly Agent.

I can explain tokens, remote credit, payment / vending / delivery status, Pay-for-Me, Split the Meter, AutoPower and Power Desk. I can look up YOUR purchases — never anyone else's.

I will never ask you to paste a 20-digit STS token here, and I will never send one in this chat. Tokens only work on the meter they were bought for.

Messages with me are processed by Meterly to answer you (not end-to-end encrypted). Tap “Talk to a person” for an end-to-end encrypted chat with a Meterly operator — Meterly cannot read those messages.`

const escalateHint = `

If you'd rather talk to a person, tap “Talk to a person”. That chat is end-to-end encrypted.`

// Welcome is the first AGENT message on a new thread.
func Welcome() string { return welcome }

// Answer is a deterministic, non-network support agent. It never invents
// delivery confirmation and never includes tokens.
func Answer(question string, txns []TxnSummary) Reply {
	q := strings.TrimSpace(question)
	if q == "" {
		return Reply{Body: "Tell me what you need help with — a purchase status, a token, Pay-for-Me, Split the Meter, AutoPower, or talking to a person." + escalateHint, MatchedIntent: "empty"}
	}
	lower := strings.ToLower(q)

	if looksLikeToken(q) {
		return Reply{
			Body:          "Please don't paste STS tokens into chat. Open Buy Power, select the purchase, and use Reveal token there. A token only works on the meter it was bought for — sharing it never moves credit to another meter.",
			MatchedIntent: "token_paste",
		}
	}

	if mentionsHuman(lower) {
		return Reply{
			Body:          "I can connect you to a Meterly operator. Tap “Talk to a person” — your next messages are end-to-end encrypted on this device. Meterly's servers cannot read them. I'll stay available on this thread's earlier agent history.",
			SuggestHuman:  true,
			MatchedIntent: "human",
		}
	}

	if r, ok := lookupTransaction(lower, txns); ok {
		return r
	}

	switch {
	case containsAny(lower, "unknown", "stuck", "pending", "status", "not arrived", "not delivered", "hasn't come", "hasnt come"):
		return Reply{Body: statusExplainer(txns) + escalateHint, MatchedIntent: "status"}
	case containsAny(lower, "refund", "money back", "reversed"):
		return Reply{Body: "Meterly refunds only after vending has definitely failed. If vending is UNKNOWN, we do not refund automatically — the token or remote credit may still have gone out. An operator can refund from MANUAL_REVIEW only when vending is NOT_STARTED or FAILED." + escalateHint, MatchedIntent: "refund"}
	case containsAny(lower, "remote", "no token", "keypad", "ciu", "accepted"):
		return Reply{Body: "Remote credit means the DisCo / meter system accepted a command. “Accepted” is not “delivered”. Delivery is CONFIRMED only when the meter itself reports the credit. We never show remote ACCEPTED as done. If your meter is not remote-capable, you get a 20-digit STS token instead.", MatchedIntent: "remote"}
	case containsAny(lower, "token", "20-digit", "20 digit", "sts"):
		return Reply{Body: "For keypad meters Meterly vends a 20-digit STS token. Reveal it on Buy Power (purchaser only). It only works on that meter; sharing a token never transfers credit. We do not put tokens in this chat.", MatchedIntent: "token"}
	case containsAny(lower, "pay for me", "pay-for-me", "payforme", "share a link", "qr"):
		return Reply{Body: "Pay-for-Me lets you create a FIXED or RANGE link with a QR code. The payer checks out without logging in and never sees your address or the STS token — the token goes to you, the requester. If your meter access is revoked the link becomes unusable.", MatchedIntent: "pay_for_me"}
	case containsAny(lower, "split", "pot", "contribute", "funding"):
		return Reply{Body: "Split the Meter is a Funding Pot. Contributors see a progress bar and initials only (never full names). When the target is reached Meterly vends once. Extra money and failed vends are refunded; UNKNOWN vends are not refunded until reconciled.", MatchedIntent: "split"}
	case containsAny(lower, "autopower", "auto recharge", "auto-recharge", "saved card", "mandate"):
		return Reply{Body: "AutoPower charges a card you saved after explicit consent. Weekly/monthly runs use Africa/Lagos time, a monthly cap, and a Skip Once button. Low-balance mode only appears when remote reading is confirmed. Three failures pause the rule.", MatchedIntent: "autopower"}
	case containsAny(lower, "landlord", "power desk", "tenant", "bulk"):
		return Reply{Body: "Landlord Power Desk is for organisations (OWNER / MANAGER / VIEWER). Bulk top-up is one payment that funds many units. History and tenant tools wait until an operator verifies the unit meter. Statements labelled “electricity purchased” come from the ledger, not from guesses.", MatchedIntent: "power_desk"}
	case containsAny(lower, "ussd", "*347", "whatsapp", "offline"):
		return Reply{Body: "Meterly Offline uses USSD (*347*88# in the simulator) and WhatsApp. Purchases still go through the same payment → vend → delivery engine. “Last token” resends are capped and audited, and only your own purchases.", MatchedIntent: "offline"}
	case containsAny(lower, "hello", "hi", "hey", "help"):
		return Reply{Body: welcome, MatchedIntent: "hello"}
	default:
		body := "I didn't match a specific topic. I can help with purchase status, tokens vs remote credit, refunds, Pay-for-Me, Split the Meter, AutoPower, Power Desk and Offline (USSD / WhatsApp)."
		if len(txns) > 0 {
			body += "\n\n" + recentSummary(txns)
		}
		return Reply{Body: body + escalateHint, SuggestHuman: true, MatchedIntent: "fallback"}
	}
}

func looksLikeToken(q string) bool {
	digits := 0
	for _, r := range q {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	return digits >= 20
}

func mentionsHuman(lower string) bool {
	return containsAny(lower, "real person", "human", "operator", "agent person", "talk to a person", "speak to", "customer service", "live chat", "representative")
}

func containsAny(lower string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(lower, n) {
			return true
		}
	}
	return false
}

func lookupTransaction(lower string, txns []TxnSummary) (Reply, bool) {
	for _, t := range txns {
		id := strings.ToLower(t.ID)
		if len(id) >= 8 && strings.Contains(lower, id) {
			return Reply{Body: formatTxn(t) + escalateHint, MatchedIntent: "txn_lookup"}, true
		}
	}
	if containsAny(lower, "my purchase", "my transaction", "last purchase", "last transaction", "my token") && len(txns) > 0 {
		return Reply{Body: formatTxn(txns[0]) + escalateHint, MatchedIntent: "txn_latest"}, true
	}
	return Reply{}, false
}

func statusExplainer(txns []TxnSummary) string {
	b := "Meterly tracks three things separately:\n" +
		"• Payment — did the PSP confirm the exact quote amount?\n" +
		"• Vending — did we receive a token or send a remote command? Timeouts are UNKNOWN, never FAILED.\n" +
		"• Delivery — TOKEN is confirmed when vended; REMOTE is confirmed only when the meter says so. ACCEPTED is not delivered.\n"
	if len(txns) > 0 {
		b += "\n" + recentSummary(txns)
	} else {
		b += "\nI don't see any purchases on this account yet."
	}
	return b
}

func recentSummary(txns []TxnSummary) string {
	n := len(txns)
	if n > 3 {
		n = 3
	}
	var b strings.Builder
	b.WriteString("Your most recent purchases:\n")
	for i := 0; i < n; i++ {
		t := txns[i]
		fmt.Fprintf(&b, "• %s — %s (payment %s, vending %s, delivery %s)\n", shortID(t.ID), t.Status, t.Payment, t.Vending, t.Delivery)
	}
	return strings.TrimRight(b.String(), "\n")
}

func formatTxn(t TxnSummary) string {
	gift := ""
	if t.IsGift {
		gift = " This was a gift purchase."
	}
	return fmt.Sprintf(
		"Purchase %s is %s.%s\nPayment: %s. Vending: %s. Delivery: %s (%s).\nAmount: ₦%.2f.\nStarted: %s.\n\nIf vending is UNKNOWN we are reconciling with the provider — we will not guess, and we will not refund while electricity may have been vended.",
		shortID(t.ID), t.Status, gift, t.Payment, t.Vending, t.Delivery, t.DeliveryMethod,
		float64(t.AmountMinor)/100.0, t.CreatedAt,
	)
}

func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
