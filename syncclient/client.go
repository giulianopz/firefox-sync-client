package syncclient

import (
	"bytes"
	"crypto/dsa"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"ffsyncclient/cli"
	"ffsyncclient/consts"
	"ffsyncclient/langext"
	"fmt"
	"github.com/golang-jwt/jwt/v4"
	"github.com/joomcode/errorx"
	"io"
	"net/http"
	"time"
)

type FxAClient struct {
	authURL string
	client  http.Client
}

func NewFxAClient(serverurl string) *FxAClient {
	return &FxAClient{
		authURL: serverurl,
		client: http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (f FxAClient) Login(ctx *cli.FFSContext, email string, password string) (FxASession, error) {
	stretchpwd := stretchPassword(email, password)

	ctx.PrintVerbose("StretchPW       := " + hex.EncodeToString(stretchpwd))

	authPW, err := deriveKey(stretchpwd, "authPW", 32)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to derive key")
	}

	ctx.PrintVerbose("AuthPW          := " + hex.EncodeToString(authPW))

	body := loginRequestSchema{
		Email:  email,
		AuthPW: hex.EncodeToString(authPW), //lowercase
		Reason: "login",
	}

	bytesBody, err := json.Marshal(body)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to marshal body")
	}

	url := f.authURL + "/account/login?keys=true"

	//TODO [unblockCode, verificationMethod] (2FA ?)

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewBuffer(bytesBody))
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to create request")
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "firefox-sync-client/"+consts.FFSCLIENT_VERSION)
	req.Header.Add("Accept", "application/json")

	ctx.PrintVerbose("Request session from " + url)

	rawResp, err := f.client.Do(req)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to do request")
	}

	respBodyRaw, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to read response-body request")
	}

	//TODO statuscode [429, 500, 503] means retry-after

	if rawResp.StatusCode != 200 {
		return FxASession{}, errorx.InternalError.New(fmt.Sprintf("call to /login returned statuscode %v\n\n%v", rawResp.StatusCode, string(respBodyRaw)))
	}

	ctx.PrintVerbose(fmt.Sprintf("Request returned statuscode %d", rawResp.StatusCode))
	ctx.PrintVerbose(fmt.Sprintf("Request returned body:\n%v", string(respBodyRaw)))

	var resp loginResponseSchema
	err = json.Unmarshal(respBodyRaw, &resp)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to unmarshal response:\n"+string(respBodyRaw))
	}

	kft, err := hex.DecodeString(resp.KeyFetchToken)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to read KeyFetchToken: "+resp.KeyFetchToken)
	}

	st, err := hex.DecodeString(resp.SessionToken)
	if err != nil {
		return FxASession{}, errorx.Decorate(err, "failed to read SessionToken: "+resp.SessionToken)
	}

	ctx.PrintVerbose("UserID          := " + resp.UserID)
	ctx.PrintVerbose("SessionToken    := " + hex.EncodeToString(st))
	ctx.PrintVerbose("KeyFetchToken   := " + hex.EncodeToString(kft))

	return FxASession{
		URL:               f.authURL,
		Email:             email,
		StretchPassword:   stretchpwd,
		UserId:            resp.UserID,
		SessionToken:      st,
		KeyFetchToken:     kft,
		SessionUpdateTime: time.Now(),
	}, nil
}

func (f FxAClient) FetchKeys(ctx *cli.FFSContext, session FxASession) ([]byte, []byte, error) {

	ctx.PrintVerbose("Request keys from " + "/account/keys")

	binResp, hawkBundleKey, err := f.requestWithHawkToken(ctx, "GET", "/account/keys", nil, session.KeyFetchToken, "keyFetchToken")
	if err != nil {
		return nil, nil, errorx.Decorate(err, "Failed to query account keys")
	}

	var resp keysResponseSchema
	err = json.Unmarshal(binResp, &resp)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to unmarshal response:\n"+string(binResp))
	}

	ctx.PrintVerbose(fmt.Sprintf("Bundle          := %v", resp.Bundle))

	bundle, err := hex.DecodeString(resp.Bundle)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to decode Bundle: "+resp.Bundle)
	}

	keys, err := unbundle("account/keys", hawkBundleKey, bundle)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to unbundle")
	}

	ctx.PrintVerbose(fmt.Sprintf("Keys<unbundled> := %v", hex.EncodeToString(keys)))

	unwrapKey, err := deriveKey(session.StretchPassword, "unwrapBkey", 32)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to derive-key")
	}

	ctx.PrintVerbose(fmt.Sprintf("Keys<unwrapped> := %v", hex.EncodeToString(unwrapKey)))

	kLow := keys[:32]
	kHigh := keys[32:]

	keyA := kLow
	keyB, err := langext.BytesXOR(kHigh, unwrapKey)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to xor key-b")
	}

	return keyA, keyB, nil
}

func (f FxAClient) ListCollections(ctx *cli.FFSContext, session HawkSession) ([]any, error) {

	binResp, err := f.request(ctx, session, "GET", "/info/collections", nil)
	if err != nil {
		return nil, errorx.Decorate(err, "API request failed")
	}

	var resp collectionsInfoResponse
	err = json.Unmarshal(binResp, &resp)
	if err != nil {
		return nil, errorx.Decorate(err, "failed to unmarshal response:\n"+string(binResp))
	}

	panic(resp) //TODO

}

func (f FxAClient) HawkAuth(ctx *cli.FFSContext, session FxASessionExt) (HawkSession, error) {
	ctx.PrintVerbose("Authenticate HAWK")

	bid, t0, err := f.getBrowserIDAssertion(ctx, session)
	if err != nil {
		return HawkSession{}, errorx.Decorate(err, "Failed to assert BID")
	}

	ctx.PrintVerbose("BID-Assertion   := " + bid)

	sessionState := session.State()

	ctx.PrintVerbose("Session-State   := " + sessionState)

	cred, err := f.getHawkCredentials(ctx, t0, bid, sessionState)
	if err != nil {
		return HawkSession{}, errorx.Decorate(err, "Failed to get hawk credentials")
	}

	return session.Extend(cred), nil
}

func (f FxAClient) getBrowserIDAssertion(ctx *cli.FFSContext, session FxASessionExt) (string, time.Time, error) {

	params := dsa.Parameters{}
	err := dsa.GenerateParameters(&params, rand.Reader, dsa.L1024N160)
	if err != nil {
		return "", time.Time{}, errorx.Decorate(err, "Failed to generate DSA params")
	}

	var privateKey dsa.PrivateKey
	privateKey.PublicKey.Parameters = params

	err = dsa.GenerateKey(&privateKey, rand.Reader)
	if err != nil {
		return "", time.Time{}, errorx.Decorate(err, "Failed to generate DSA key-pair")
	}

	body := signCertRequestSchema{
		PublicKey: signCertRequestSchemaPKey{
			Algorithm: "DS",
			P:         privateKey.P.Text(16),
			Q:         privateKey.Q.Text(16),
			G:         privateKey.G.Text(16),
			Y:         privateKey.Y.Text(16),
		},
		Duration: consts.DefaultBIDAssertionDuration * 1000,
	}

	ctx.PrintVerbose("Sign new certificate via " + "/certificate/sign")

	binResp, _, err := f.requestWithHawkToken(ctx, "POST", "/certificate/sign", body, session.SessionToken, "sessionToken")
	if err != nil {
		return "", time.Time{}, errorx.Decorate(err, "Failed to sign cert")
	}

	var resp signCertResponseSchema
	err = json.Unmarshal(binResp, &resp)
	if err != nil {
		return "", time.Time{}, errorx.Decorate(err, "failed to unmarshal response:\n"+string(binResp))
	}

	ctx.PrintVerbose(fmt.Sprintf("Cert :=\n%v", resp.Certificate))

	t0 := time.Now()
	exp := t0.UnixMilli() + consts.DefaultBIDAssertionDuration*1000

	token := jwt.NewWithClaims(&SigningMethodDS128{}, jwt.MapClaims{
		"exp": exp,
		"aud": consts.TokenServerURL,
	})

	assertion, err := token.SignedString(&privateKey)
	if err != nil {
		return "", time.Time{}, errorx.Decorate(err, "failed to generate JWT")
	}

	return resp.Certificate + "~" + assertion, t0, nil
}

func (f FxAClient) requestWithHawkToken(ctx *cli.FFSContext, method string, relurl string, body any, token []byte, tokenType string) ([]byte, []byte, error) {
	url := f.authURL + relurl

	strBody := ""
	var bodyReader io.Reader = nil
	if body != nil {
		bytesBody, err := json.Marshal(body)
		if err != nil {
			return nil, nil, errorx.Decorate(err, "failed to marshal body")
		}
		strBody = string(bytesBody)
		bodyReader = bytes.NewReader(bytesBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to create request")
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "firefox-sync-client/"+consts.FFSCLIENT_VERSION)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Host", req.URL.Host)

	ctx.PrintVerbose(fmt.Sprintf("Calculate HAWK-Auth-Token"))

	hawkAuth, hawkBundleKey, err := calcHawkTokenAuth(token, tokenType, req.Method, req.URL.String(), strBody)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to create hawk-auth")
	}

	ctx.PrintVerbose(fmt.Sprintf("HAWK-Auth-Token := %v", hawkAuth))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-Bundle-Key := %v", hex.EncodeToString(hawkBundleKey)))

	req.Header.Add("Authorization", hawkAuth)

	ctx.PrintVerbose("Do HAWK-token authenticated request to " + relurl)

	rawResp, err := f.client.Do(req)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to do request")
	}

	respBodyRaw, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return nil, nil, errorx.Decorate(err, "failed to read response-body request")
	}

	//TODO statuscode [429, 500, 503] means retry-after

	if rawResp.StatusCode != 200 {
		return nil, nil, errorx.InternalError.New(fmt.Sprintf("call to %v returned statuscode %v\n\n%v", relurl, rawResp.StatusCode, string(respBodyRaw)))
	}

	ctx.PrintVerbose(fmt.Sprintf("Request returned statuscode %d", rawResp.StatusCode))
	ctx.PrintVerbose(fmt.Sprintf("Request returned body:\n%v", string(respBodyRaw)))

	return respBodyRaw, hawkBundleKey, nil
}

func (f FxAClient) getHawkCredentials(ctx *cli.FFSContext, t0 time.Time, bid string, clientState string) (HawkCredentials, error) {
	auth := "BrowserID " + bid

	req, err := http.NewRequestWithContext(ctx, "GET", consts.TokenServerURL+"/1.0/sync/1.5", nil)
	if err != nil {
		return HawkCredentials{}, errorx.Decorate(err, "failed to create request")
	}
	req.Header.Add("Authorization", auth)
	req.Header.Add("X-Client-State", clientState)

	ctx.PrintVerbose("Query HAWK credentials")

	rawResp, err := f.client.Do(req)
	if err != nil {
		return HawkCredentials{}, errorx.Decorate(err, "failed to do request")
	}

	respBodyRaw, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return HawkCredentials{}, errorx.Decorate(err, "failed to read response-body request")
	}

	if rawResp.StatusCode != 200 {
		return HawkCredentials{}, errorx.InternalError.New(fmt.Sprintf("api call returned statuscode %v\n\n%v", rawResp.StatusCode, string(respBodyRaw)))
	}

	var resp hawkCredResponseSchema
	err = json.Unmarshal(respBodyRaw, &resp)
	if err != nil {
		return HawkCredentials{}, errorx.Decorate(err, "failed to unmarshal response:\n"+string(respBodyRaw))
	}

	ctx.PrintVerbose(fmt.Sprintf("HAWK-ID         := %v", resp.ID))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-Key        := %v", resp.Key))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-UserID     := %v", resp.UID))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-Endpoint   := %v", resp.APIEndpoint))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-Duration   := %v", resp.Duration))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-HashAlgo   := %v", resp.HashAlgorithm))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-FxA-Uid    := %v", resp.HashedFxAUID))
	ctx.PrintVerbose(fmt.Sprintf("HAWK-NodeType   := %v", resp.NodeType))

	return HawkCredentials{
		HawkID:            resp.ID,
		HawkKey:           resp.Key,
		APIEndpoint:       resp.APIEndpoint,
		HawkDuration:      resp.Duration,
		HawkHashAlgorithm: resp.HashAlgorithm,
		HawkUpdateTime:    t0,
	}, nil
}

func (f FxAClient) request(ctx *cli.FFSContext, session HawkSession, method string, relurl string, body any) ([]byte, error) {

	url := session.APIEndpoint + relurl

	strBody := ""
	var bodyReader io.Reader = nil
	if body != nil {
		bytesBody, err := json.Marshal(body)
		if err != nil {
			return nil, errorx.Decorate(err, "failed to marshal body")
		}
		strBody = string(bytesBody)
		bodyReader = bytes.NewReader(bytesBody)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, bodyReader)
	if err != nil {
		return nil, errorx.Decorate(err, "failed to create request")
	}

	req.Header.Add("Content-Type", "application/json")
	req.Header.Add("User-Agent", "firefox-sync-client/"+consts.FFSCLIENT_VERSION)
	req.Header.Add("Accept", "application/json")
	req.Header.Add("Host", req.URL.Host)

	ctx.PrintVerbose(fmt.Sprintf("Calculate HAWK-Auth-Token"))

	hawkAuth, err := calcHawkAuth(ctx, session, req.Method, req.URL.String(), strBody, "application/json")
	if err != nil {
		return nil, errorx.Decorate(err, "failed to create hawk-auth")
	}

	req.Header.Add("Authorization", hawkAuth)

	ctx.PrintVerbose(fmt.Sprintf("Authorization   := %v", hawkAuth))

	rawResp, err := f.client.Do(req)
	if err != nil {
		return nil, errorx.Decorate(err, "failed to do request")
	}

	respBodyRaw, err := io.ReadAll(rawResp.Body)
	if err != nil {
		return nil, errorx.Decorate(err, "failed to read response-body request")
	}

	//TODO statuscode [429, 500, 503] means retry-after

	if rawResp.StatusCode != 200 {
		return nil, errorx.InternalError.New(fmt.Sprintf("call to %v returned statuscode %v\n\n%v", relurl, rawResp.StatusCode, string(respBodyRaw)))
	}

	ctx.PrintVerbose(fmt.Sprintf("Request returned statuscode %d", rawResp.StatusCode))
	ctx.PrintVerbose(fmt.Sprintf("Request returned body:\n%v", string(respBodyRaw)))

	return respBodyRaw, nil
}
