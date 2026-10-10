package spec

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"go.yaml.in/yaml/v3"
)

func Decode(data []byte) (*Document, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)

	var document *Document

	if err := decoder.Decode(&document); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("workflow YAML is empty")
		}

		return nil, fmt.Errorf("decode workflow YAML error: %w", err)
	}

	if document == nil {
		return nil, errors.New("workflow must be a YAML object")
	}

	var extra yaml.Node

	err := decoder.Decode(&extra)
	if err == nil {
		return nil, errors.New("expected exactly one YAML document")
	}
	if !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("decode trailing YAML: %w", err)
	}

	return document, nil
}
