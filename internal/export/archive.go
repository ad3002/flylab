package export

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
)

func CreateJobArchive(jobDir string, destWriter io.Writer) error {
	zipWriter := zip.NewWriter(destWriter)
	defer zipWriter.Close()

	entries, err := os.ReadDir(jobDir)
	if err != nil {
		return err
	}

	for _, entry := range entries {
		if entry.IsDir() {
			continue // skip subdirectories
		}
		path := filepath.Join(jobDir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		w, err := zipWriter.Create(entry.Name())
		if err != nil {
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
	}

	return nil
}
