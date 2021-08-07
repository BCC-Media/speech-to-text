package stt

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func Test_fmtDuration(t *testing.T) {
	var d time.Duration = 1000000000 * 60 * 91
	assert.Equal(t, fmtDuration(d, 100), "01:31:00:00")

	d = 1000000000 * 60 * 31
	assert.Equal(t, fmtDuration(d, 100), "00:31:00:00")
}
