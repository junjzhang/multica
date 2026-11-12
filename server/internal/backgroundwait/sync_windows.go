package backgroundwait

// Windows does not support fsync on directory handles. WritePrivate flushes
// the file before publishing it; rename durability follows the filesystem.
func syncDir(string) error { return nil }
