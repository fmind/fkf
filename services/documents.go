package services

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"hash"

	"github.com/fmind/fkf/core"
	"github.com/fmind/fkf/sources"
)

func appendUnique(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

// eachDocument visits every collected document in stable layer and URI order.
func eachDocument(ctx context.Context, base *Base, visit func(*sources.Document) error) error {
	if base.Store.Enabled(core.LayerEvents) {
		dates, err := base.EventDates()
		if err != nil {
			return err
		}
		for _, date := range dates {
			names, err := base.DayDocuments(date)
			if err != nil {
				return err
			}
			for _, name := range names {
				if err := visitDocument(ctx, base, sources.EventDocumentURI(date, name), visit); err != nil {
					return err
				}
			}
		}
	}
	if base.Store.Enabled(core.LayerIndex) {
		names, err := base.IndexDocuments()
		if err != nil {
			return err
		}
		for _, name := range names {
			if err := visitDocument(ctx, base, sources.IndexDocumentURI(name), visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func visitDocument(ctx context.Context, base *Base, uri string, visit func(*sources.Document) error) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	document, err := base.ReadDocumentContext(ctx, uri)
	if err != nil {
		return err
	}
	return visit(document)
}

// collectedDocumentsSHA256 identifies the complete canonical collected-document snapshot.
func collectedDocumentsSHA256(ctx context.Context, base *Base) (string, error) {
	digest := sha256.New()
	_, _ = digest.Write([]byte("fkf-collected-documents-v1\x00"))
	err := eachDocument(ctx, base, func(document *sources.Document) error {
		encoded, err := sources.EncodeDocument(document)
		if err != nil {
			return err
		}
		writeDigestValue(digest, []byte(document.URI()))
		writeDigestValue(digest, encoded)
		return nil
	})
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest.Sum(nil)), nil
}

func writeDigestValue(digest hash.Hash, value []byte) {
	var size [8]byte
	binary.BigEndian.PutUint64(size[:], uint64(len(value)))
	_, _ = digest.Write(size[:])
	_, _ = digest.Write(value)
}
