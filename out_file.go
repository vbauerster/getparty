package getparty

import "os"

const umask = 0644

// outFile invariant:
// must not implement io.ReaderFrom for correct buf usage by io.CopyBuffer
type outFile struct {
	name string
	file *os.File
}

func (s *outFile) Open(flag int) error {
	err := s.Close()
	if err != nil {
		return err
	}
	s.file, err = os.OpenFile(s.name, flag, umask)
	return err
}

func (s *outFile) Close() error {
	if s != nil && s.file != nil {
		return s.file.Close()
	}
	return nil
}

func (s *outFile) Sync() error {
	if s.file != nil {
		return s.file.Sync()
	}
	return nil
}

func (s *outFile) Write(b []byte) (int, error) {
	return s.file.Write(b)
}

func (s *outFile) Truncate(size int64) error {
	return s.file.Truncate(size)
}

func (s *outFile) Stat() (os.FileInfo, error) {
	if s.file != nil {
		return s.file.Stat()
	}
	return os.Stat(s.name)
}

func (s *outFile) Name() string {
	if s.file != nil {
		return s.file.Name()
	}
	return s.name
}

func (s *outFile) String() string {
	return s.Name()
}

func (s *outFile) checkContentMismatch(written int64) error {
	stat, err := s.Stat()
	if err != nil {
		return err
	}
	if size := stat.Size(); written != size {
		err := ContentMismatchError[int64]{
			kind: "Length",
			old:  written,
			new:  size,
		}
		return withMessage(err, s.Name())
	}
	return nil
}
