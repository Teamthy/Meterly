package httpapi

import (
	"bytes"
	"strings"
	"testing"

	"github.com/teamthy/meterly/internal/receipts"
)

func TestReceiptHTMLIsEscaped(t *testing.T) {
	var out bytes.Buffer
	if err := receiptHTML.Execute(&out, receipts.Receipt{Disco: `<script>alert(1)</script>`, Currency: "NGN", TotalMinor: 12345}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "<script>") || !strings.Contains(out.String(), "&lt;script&gt;") || !strings.Contains(out.String(), "123.45") {
		t.Fatal("HTML escaping or amount rendering failed")
	}
}
