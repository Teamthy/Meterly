package privacy

import (
	"time"

	"github.com/google/uuid"
)

const (
	ExportPending    = "PENDING"
	ExportReady      = "READY"
	ExportDownloaded = "DOWNLOADED"
	ExportExpired    = "EXPIRED"
	ExportFailed     = "FAILED"

	DefaultExportTTL = 24 * time.Hour
)

// ExportJob is one subject-access archive. Ciphertext fields are the encrypted
// JSON and the download token; they are never logged.
type ExportJob struct {
	ID                uuid.UUID
	UserID            string
	Day               string // YYYY-MM-DD UTC
	Status            string
	ArchiveCiphertext []byte
	ArchiveKeyVersion string
	TokenHMAC         string
	TokenCiphertext   []byte
	TokenKeyVersion   string
	ExpiresAt         time.Time
	DownloadedAt      *time.Time
	Error             string
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func ExportDay(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

func CloneExport(j ExportJob) ExportJob {
	out := j
	if j.ArchiveCiphertext != nil {
		out.ArchiveCiphertext = append([]byte(nil), j.ArchiveCiphertext...)
	}
	if j.TokenCiphertext != nil {
		out.TokenCiphertext = append([]byte(nil), j.TokenCiphertext...)
	}
	if j.DownloadedAt != nil {
		t := *j.DownloadedAt
		out.DownloadedAt = &t
	}
	return out
}
