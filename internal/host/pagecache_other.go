//go:build !linux

package host

// dropPageCache is a no-op off Linux: posix_fadvise is not available on macOS
// or Windows. Nothing is dropped and nothing is logged.
func dropPageCache(string) (evicted int64, measured bool, err error) {
	return 0, false, nil
}
