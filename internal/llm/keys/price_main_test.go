package keys

import (
	"os"
	"testing"
)

// The tests were written for the reference price of 0.1 per million tokens; what they check is the conversion, whichever price is configured.
func TestMain(m *testing.M) {
	DollarsPerMTokens = 0.1
	os.Exit(m.Run())
}
