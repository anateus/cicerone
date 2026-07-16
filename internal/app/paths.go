package app

import "path/filepath"

type Paths struct {
	DataDir  string
	CacheDir string
	DBPath   string
}

func DefaultPaths(home string) Paths {
	data := filepath.Join(home, "Library", "Application Support", "cicerone")
	return Paths{
		DataDir:  data,
		CacheDir: filepath.Join(home, "Library", "Caches", "cicerone"),
		DBPath:   filepath.Join(data, "cicerone.db"),
	}
}
