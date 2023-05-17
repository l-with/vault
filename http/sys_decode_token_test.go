// Copyright (c) HashiCorp, Inc.
// SPDX-License-Identifier: MPL-2.0

package http

import (
	"testing"

	"github.com/go-test/deep"
	"github.com/hashicorp/vault/sdk/helper/roottoken"
	"github.com/hashicorp/vault/vault"
)

func TestDecodeTokenNoPayload(t *testing.T) {
	core, _, _ := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()

	resp := testHttpPut(t, "", addr+"/v1/sys/decode-token", nil)
	testResponseStatus(t, resp, 500)
}

func TestDecodeTokenNoOTP(t *testing.T) {
	core, _, _ := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()

	resp := testHttpPut(t, "", addr+"/v1/sys/decode-token", map[string]interface{}{
		"encoded_token": "token",
	})
	testResponseStatus(t, resp, 500)
}

func TestDecodeTokenNoEncodedToken(t *testing.T) {
	core, _, _ := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)
	defer ln.Close()

	resp := testHttpPut(t, "", addr+"/v1/sys/decode-token", map[string]interface{}{
		"otp": "otp",
	})
	testResponseStatus(t, resp, 500)
}

func TestDecodeToken(t *testing.T) {
	core, _, _ := vault.TestCoreUnsealed(t)
	ln, addr := TestServer(t, core)

	defer ln.Close()

	token := "someToken"
	otpLength := len(token)
	otp, err := roottoken.GenerateOTP(otpLength)
	if err != nil {
		t.Fatal(err.Error())
	}
	encodedToken, err := roottoken.EncodeToken(token, otp)
	resp := testHttpPut(t, "", addr+"/v1/sys/decode-token", map[string]interface{}{
		"encoded_token": encodedToken,
		"otp":           otp,
	})
	var actual map[string]interface{}
	expected := map[string]interface{}{
		"token": token,
	}
	testResponseStatus(t, resp, 200)
	testResponseBody(t, resp, &actual)
	if diff := deep.Equal(actual["data"], expected); diff != nil {
		t.Fatal(diff)
	}
}