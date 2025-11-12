package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
)

func installHostBinary(logger *slog.Logger, binaryName string) error {
	srcPath := filepath.Join("/app", binaryName)
	destDir := filepath.Join(*hostRoot, *cniBinDir)
	destPath := filepath.Join(destDir, binaryName)

	if filesMatch(srcPath, destPath) {
		logger.Debug("Binary already up to date", slog.String("binary", binaryName))
		return nil
	}

	logger.Info("Installing CNI binary",
		slog.String("binary", binaryName),
		slog.String("source", srcPath),
		slog.String("destination", destPath),
	)

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", destDir, err)
	}

	srcFile, err := os.Open(srcPath)
	if err != nil {
		return fmt.Errorf("failed to open source file %s: %w", srcPath, err)
	}
	defer srcFile.Close()

	tmpPath := destPath + ".tmp"
	if err := copyFile(srcFile, tmpPath); err != nil {
		return fmt.Errorf("failed to copy file: %w", err)
	}

	if err := os.Chmod(tmpPath, 0o755); err != nil {
		return fmt.Errorf("failed to set permissions: %w", err)
	}

	if err := os.Rename(tmpPath, destPath); err != nil {
		return fmt.Errorf("failed to rename file: %w", err)
	}

	logger.Info("Binary installed successfully", slog.String("binary", binaryName))
	return nil
}

func copyFile(source io.Reader, targetPath string) error {
	outFile, err := os.Create(targetPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer outFile.Close()

	limitedReader := &io.LimitedReader{
		R: source,
		N: maxCopyBytes,
	}

	_, err = io.Copy(outFile, limitedReader)
	if err != nil {
		return fmt.Errorf("failed to write file: %w", err)
	}

	if limitedReader.N <= 0 {
		return fmt.Errorf("reached uncompressed file size limit %d bytes", maxCopyBytes)
	}

	return nil
}

func filesMatch(src, dest string) bool {
	srcInfo, err := os.Stat(src)
	if err != nil {
		return false
	}

	destInfo, err := os.Stat(dest)
	if err != nil {
		return false
	}

	return srcInfo.Size() == destInfo.Size() && srcInfo.ModTime().Equal(destInfo.ModTime())
}
