package server

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/storage"
)

func TestSecureRenewalAndActivationUsePresentedTLSLeaf(t *testing.T) {
	_, _, _, first, second, srv, firstClient, _ := secureFixture(t, false)
	body, err := json.Marshal(storage.RenewalRequest{RequestID: uuid.NewString(), CSRPEM: second.request.CSRPEM})
	if err != nil {
		t.Fatal(err)
	}
	response, err := firstClient.Post(srv.URL+"/v1/enrollment/renew", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var issued storage.EnrollmentResponse
	err = json.NewDecoder(response.Body).Decode(&issued)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || err != nil || issued.NodeID != first.response.NodeID || issued.Serial == first.response.Serial {
		t.Fatalf("renewal failed: status=%d decode=%v", response.StatusCode, err)
	}
	leaf, _ := pem.Decode([]byte(issued.CertificatePEM))
	if leaf == nil {
		t.Fatal("renewal certificate missing")
	}
	pending := tls.Certificate{Certificate: [][]byte{leaf.Bytes}, PrivateKey: second.cert.PrivateKey}
	for _, check := range []struct {
		client *http.Client
		serial string
	}{
		{firstClient, first.response.Serial},
		{secureHTTPClient(t, srv, &pending), issued.Serial},
	} {
		response, err := check.client.Post(srv.URL+"/v1/enrollment/activate", "application/json", nil)
		if err != nil {
			t.Fatal(err)
		}
		var node storage.AuthorizedSecurityNode
		err = json.NewDecoder(response.Body).Decode(&node)
		response.Body.Close()
		if response.StatusCode != http.StatusOK || err != nil || node.CurrentSerial != check.serial {
			t.Fatalf("activation ignored presented leaf: status=%d current=%s decode=%v", response.StatusCode, node.CurrentSerial, err)
		}
	}
}
