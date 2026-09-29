package swh

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const (
	dirMode   = 0o700
	fileMode  = 0o600
	gzipMagic = "\x1f\x8b"
)

func extract(input io.Reader, dest, root string, limit int64) error {
	buffer := bufio.NewReader(input)
	magic, err := buffer.Peek(len(gzipMagic))
	if err != nil {
		return err
	}
	var reader io.Reader = buffer
	if string(magic) == gzipMagic {
		gz, err := gzip.NewReader(buffer)
		if err != nil {
			return err
		}
		defer func() { _ = gz.Close() }()
		reader = gz
	}
	bounded := &io.LimitedReader{R: reader, N: limit}
	archive := tar.NewReader(bounded)
	for {
		header, err := archive.Next()
		if errors.Is(err, io.EOF) {
			return finishArchive(reader, bounded)
		}
		if err != nil {
			return fmt.Errorf("incomplete archive or max-bytes exceeded: %w", err)
		}
		if err := extractEntry(archive, header, dest, root, bounded.N); err != nil {
			return err
		}
	}
}

func finishArchive(reader io.Reader, bounded *io.LimitedReader) error {
	if _, err := io.Copy(io.Discard, bounded); err != nil {
		return err
	}
	if bounded.N > 0 {
		return nil
	}
	var extra [1]byte
	n, err := io.ReadFull(reader, extra[:])
	if n > 0 {
		return errors.New("bundle exceeds max-bytes")
	}
	if errors.Is(err, io.EOF) {
		return nil
	}
	return err
}

func extractEntry(reader io.Reader, header *tar.Header, dest, root string, remaining int64) error {
	name := path.Clean(header.Name)
	if !filepath.IsLocal(name) || (name != root && !strings.HasPrefix(name, root+"/")) {
		return fmt.Errorf("unsafe archive path %q", header.Name)
	}
	if header.Typeflag != tar.TypeDir && header.Typeflag != tar.TypeReg {
		return fmt.Errorf("unsupported archive entry %q", header.Name)
	}
	if strings.EqualFold(name, root+"/objects/info/alternates") || strings.EqualFold(name, root+"/objects/info/http-alternates") {
		return errors.New("bundle contains external object alternates")
	}
	if strings.EqualFold(name, root+"/commondir") {
		return errors.New("bundle contains external common directory")
	}
	target := filepath.Join(dest, filepath.FromSlash(name))
	if header.Typeflag == tar.TypeDir {
		return os.MkdirAll(target, dirMode)
	}
	if header.Size > remaining {
		return errors.New("bundle exceeds max-bytes")
	}
	if err := os.MkdirAll(filepath.Dir(target), dirMode); err != nil {
		return err
	}
	file, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, fileMode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(file, reader)
	return errors.Join(copyErr, file.Close())
}
