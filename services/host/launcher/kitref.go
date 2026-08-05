package launcher

// KitRef is the git ref THIS build pins to: the release tag for a stamped
// release, "main" otherwise. It sits beside Version and IsReleased because all
// three answer the same question — which build am I, and what does that build
func KitRef(version string) string {
	if IsReleased(version) {
		return "v" + version
	}
	return "main"
}
