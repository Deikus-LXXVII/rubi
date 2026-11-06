package home

import "errors"

func mkfifo(string, uint32) error { return errors.New("no named pipes of this kind on Windows") }
