package terminalscreen

func getOrDefault[T any](slice []T, index int) (result T) {
	if index < len(slice) {
		result = slice[index]
	}
	return result
}

func ensureAtLeastLength[T any](slice []T, atLeastLength int) []T {
	if len(slice) < atLeastLength {
		slice = append(slice, make([]T, atLeastLength-len(slice))...)
	}
	return slice
}
