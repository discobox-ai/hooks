package processhelper

func helperExecutable() (string, error) {
	// Execute the running image, even if its original pathname has been unlinked
	// or replaced. Do not resolve this symlink back to that pathname.
	return "/proc/self/exe", nil
}
