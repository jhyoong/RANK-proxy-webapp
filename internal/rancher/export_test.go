package rancher

import "testing"

// SetMaxListBody overrides the cluster list body limit for one test.
func SetMaxListBody(t testing.TB, n int64) {
	t.Helper()
	old := maxListBody
	maxListBody = n
	t.Cleanup(func() { maxListBody = old })
}
