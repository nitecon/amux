package storage

import (
	"context"
	"reflect"
	"testing"
)

func assertReclaimValue(t *testing.T, s *Store, query string, want any, args ...any) {
	t.Helper()
	var got any
	if err := s.db.QueryRowContext(context.Background(), query, args...).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query %q = %#v, want %#v", query, got, want)
	}
}
