// Command rekey decrypts ciphertext with TOKEN_ENC_KEY and re-encrypts with
// TOKEN_ENC_KEY_NEW / KEY_VERSION (default v2). HMAC key is unchanged.
//
//	TOKEN_ENC_KEY=... TOKEN_ENC_KEY_NEW=... KEY_VERSION=v2 DATABASE_URL=... go run ./cmd/rekey
package main

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/teamthy/meterly/internal/rekey"
	"github.com/teamthy/meterly/internal/secure"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	url := os.Getenv("DATABASE_URL")
	oldHex := strings.TrimSpace(os.Getenv("TOKEN_ENC_KEY"))
	newHex := strings.TrimSpace(os.Getenv("TOKEN_ENC_KEY_NEW"))
	hmacHex := strings.TrimSpace(os.Getenv("METER_HMAC_KEY"))
	oldVer := strings.TrimSpace(os.Getenv("KEY_VERSION_OLD"))
	newVer := strings.TrimSpace(os.Getenv("KEY_VERSION"))
	if oldVer == "" {
		oldVer = "v1"
	}
	if newVer == "" {
		newVer = "v2"
	}
	if url == "" || oldHex == "" || newHex == "" || hmacHex == "" {
		log.Error("DATABASE_URL, TOKEN_ENC_KEY, TOKEN_ENC_KEY_NEW and METER_HMAC_KEY are required")
		os.Exit(1)
	}
	if oldHex == newHex {
		log.Error("TOKEN_ENC_KEY_NEW must differ from TOKEN_ENC_KEY")
		os.Exit(1)
	}
	oldKey, err := hex.DecodeString(oldHex)
	if err != nil || len(oldKey) != 32 {
		log.Error("TOKEN_ENC_KEY must be 64 hex characters")
		os.Exit(1)
	}
	newKey, err := hex.DecodeString(newHex)
	if err != nil || len(newKey) != 32 {
		log.Error("TOKEN_ENC_KEY_NEW must be 64 hex characters")
		os.Exit(1)
	}
	hmacKey, err := hex.DecodeString(hmacHex)
	if err != nil || len(hmacKey) != 32 {
		log.Error("METER_HMAC_KEY must be 64 hex characters")
		os.Exit(1)
	}
	oldB, err := secure.NewBox(oldKey, hmacKey, oldVer)
	if err != nil {
		log.Error("old box", "err", err)
		os.Exit(1)
	}
	newB, err := secure.NewBox(newKey, hmacKey, newVer)
	if err != nil {
		log.Error("new box", "err", err)
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		log.Error("connect", "err", err)
		os.Exit(1)
	}
	defer pool.Close()
	rep, err := rekey.Run(ctx, pool, oldB, newB)
	if err != nil {
		log.Error("rekey", "err", err)
		os.Exit(1)
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(rep)
}
