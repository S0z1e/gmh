package session

import "os"

// Windows does not support os.File.Sync on a directory handle. Recovery
// flushes the backup file with FlushFileBuffers before truncating the source;
// it cannot additionally promise POSIX directory-fsync durability here.
func syncRecoveryDirectory(string) error { return nil }

func syncRootDirectory(*os.Root) error { return nil }
