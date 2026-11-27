package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/nitecon/amux/internal/clientconfig"
	"github.com/nitecon/amux/internal/clientcontrol"
)

type clientReload struct {
	change *clientcontrol.CheckoutChange
	reply  chan string
}

func serveClientControl(config, endpoint, build string, reloads chan<- clientReload, done <-chan struct{}, register func(*http.ServeMux) func()) (func(), error) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var secret [32]byte
	if _, err := rand.Read(secret[:]); err != nil {
		listener.Close()
		return nil, err
	}
	token := hex.EncodeToString(secret[:])
	path := config + ".control"
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if os.IsExist(err) {
		if err = os.Remove(path); err == nil {
			file, err = os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		}
	}
	if err != nil {
		listener.Close()
		return nil, err
	}
	_, err = fmt.Fprintf(file, "%s\n%s\n", listener.Addr(), token)
	closeErr := file.Close()
	if err != nil || closeErr != nil {
		listener.Close()
		os.Remove(path)
		return nil, fmt.Errorf("write client control address: %v %v", err, closeErr)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(clientcontrol.BuildHeader, build)
		fmt.Fprintln(w, endpoint)
	})
	apply := func(w http.ResponseWriter, r *http.Request, change *clientcontrol.CheckoutChange) {
		result, err := submitClientReload(r.Context(), reloads, done, change)
		if err != nil {
			if errors.Is(err, errClientStopping) {
				http.Error(w, err.Error(), http.StatusServiceUnavailable)
			}
			return
		}
		if strings.HasPrefix(result, "Rejected configuration:") {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
		fmt.Fprintln(w, result)
	}
	mux.HandleFunc("POST /reload", func(w http.ResponseWriter, r *http.Request) { apply(w, r, nil) })
	mux.HandleFunc("PATCH /checkout", func(w http.ResponseWriter, r *http.Request) {
		var change clientcontrol.CheckoutChange
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&change); err != nil ||
			(change.ProjectsDirectory == nil) == (change.Project == "") ||
			(change.Project == "" && change.Checkout != "") ||
			(change.Recreate && change.Checkout == "") ||
			(change.ExcludeExecution != nil && (change.Project == "" || change.Checkout != "")) {
			http.Error(w, "invalid checkout change", http.StatusBadRequest)
			return
		}
		apply(w, r, &change)
	})
	var cleanup func()
	if register != nil {
		cleanup = register(mux)
	}
	authenticated := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: authenticated, ReadHeaderTimeout: 5 * time.Second, MaxHeaderBytes: 8192}
	go srv.Serve(listener)
	return func() {
		srv.Close()
		if cleanup != nil {
			cleanup()
		}
		os.Remove(path)
	}, nil
}

var errClientStopping = errors.New("client stopping")

func submitClientReload(ctx context.Context, reloads chan<- clientReload, stopped <-chan struct{}, change *clientcontrol.CheckoutChange) (string, error) {
	reply := make(chan string, 1)
	select {
	case reloads <- clientReload{change: change, reply: reply}:
	case <-stopped:
		return "", errClientStopping
	case <-ctx.Done():
		return "", ctx.Err()
	}
	select {
	case result := <-reply:
		return result, nil
	case <-stopped:
		return "", errClientStopping
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func requestClientReload(ctx context.Context, stdout io.Writer) error {
	path, err := clientconfig.DefaultPath()
	if err != nil {
		return err
	}
	return requestClientReloadAt(ctx, path, stdout)
}

func requestClientReloadAt(ctx context.Context, path string, stdout io.Writer) error {
	body, err := clientcontrol.Request(ctx, path, http.MethodPost, "/reload", nil)
	if err != nil {
		return err
	}
	_, err = stdout.Write(body)
	return err
}
