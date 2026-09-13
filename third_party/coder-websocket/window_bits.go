//go:build !js

package websocket

import "strconv"

func parseCompressionWindowBits(value string) (int, bool) {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		value = value[1 : len(value)-1]
	}
	bits, err := strconv.Atoi(value)
	return bits, err == nil && bits >= 9 && bits <= 15 && strconv.Itoa(bits) == value
}
