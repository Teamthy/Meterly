package notificationconsumer

import "errors"

var ErrInvalidLimit = errors.New("notification candidate batch limit must be between 1 and 1000")
