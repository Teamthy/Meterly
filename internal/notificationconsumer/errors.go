package notificationconsumer

import "errors"

var ErrInvalidLimit = errors.New("notification candidate batch limit must be between 1 and 1000")

var ErrInvalidCutoff = errors.New("notification candidate cutoff is required")
