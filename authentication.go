package frame

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/dgrijalva/jwt-go/v4"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"io/ioutil"
	"log"
	"math/big"
	"net/http"
	"strings"

	"google.golang.org/grpc/metadata"
)

const ctxKeyAuthentication = "authenticationKey"
const envOauth2WellKnownJwk = "OAUTH2_WELL_KNOWN_JWK"

// AuthenticationClaims Create a struct that will be encoded to a JWT.
// We add jwt.StandardClaims as an embedded type, to provide fields like expiry time
type AuthenticationClaims struct {
	ProfileID   string   `json:"sub,omitempty"`
	TenantID    string   `json:"tenant_id,omitempty"`
	PartitionID string   `json:"partition_id,omitempty"`
	AccessID    string   `json:"access_id,omitempty"`
	Roles       []string `json:"roles,omitempty"`
	jwt.StandardClaims
}

func (a *AuthenticationClaims) isSystem() bool {
	//TODO: tokens which are granted as client credentials have no partition information attached
	// Since we cannot pass custom information to token to allow specifying who is an admin.
	// We will check if the subject starts with service. for now and make them system see: https://github.com/ory/hydra/issues/1748
	return strings.HasPrefix(a.Subject, "service_")
}

// AsMetadata Creates a string map to be used as metadata in queue data
func (a *AuthenticationClaims) AsMetadata() map[string]string {

	m := make(map[string]string)
	m["tenant_id"] = a.TenantID
	m["partition_id"] = a.PartitionID
	m["profile_id"] = a.ProfileID
	m["access_id"] = a.AccessID
	m["roles"] = strings.Join(a.Roles, ",")
	return m
}

// ClaimsToContext adds authentication claims to the current supplied context
func (a *AuthenticationClaims) ClaimsToContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, ctxKeyAuthentication, a)
}

// ClaimsFromContext extracts authentication claims from the supplied context if any exist
func ClaimsFromContext(ctx context.Context) *AuthenticationClaims {
	authenticationClaims, ok := ctx.Value(ctxKeyAuthentication).(*AuthenticationClaims)
	if !ok {
		return nil
	}

	return authenticationClaims
}

// ClaimsFromMap extracts authentication claims from the supplied map if they exist
func ClaimsFromMap(m map[string]string) *AuthenticationClaims {
	var authenticationClaims AuthenticationClaims

	if val, ok := m["tenant_id"]; ok {
		authenticationClaims = AuthenticationClaims{}
		authenticationClaims.TenantID = val

		if val, ok := m["partition_id"]; ok {
			authenticationClaims.PartitionID = val

			if val, ok := m["profile_id"]; ok {
				authenticationClaims.ProfileID = val

				if val, ok := m["access_id"]; ok {
					authenticationClaims.AccessID = val
					if val, ok := m["roles"]; ok {
						authenticationClaims.Roles = strings.Split(val, ",")
						return &authenticationClaims
					}
				}
			}
		}
	}

	return nil
}

func authenticate(ctx context.Context, jwtToken string, audience string, issuer string) (context.Context, error) {

	claims := &AuthenticationClaims{}

	var options []jwt.ParserOption

	if audience == "" {
		options = []jwt.ParserOption{jwt.WithoutAudienceValidation()}
	} else {
		options = []jwt.ParserOption{jwt.WithAudience(audience)}
	}

	if issuer != "" {
		options = append(options, jwt.WithIssuer(issuer))
	}

	token, err := jwt.ParseWithClaims(jwtToken, claims, getPemCert, options...)
	if err != nil {
		return ctx, err
	}

	if !token.Valid {
		return ctx, errors.New("supplied token was invalid")
	}

	//if !claims.VerifyAudience(audience, audience != ""){
	//	return ctx, errors.New(fmt.Sprintf("token audience does not match %s", audience))
	//}
	//
	//if !claims.VerifyIssuer(issuer, issuer != "") {
	//	return ctx, errors.New(fmt.Sprintf("token issuer does not match %s", issuer))
	//}

	ctx = claims.ClaimsToContext(ctx)

	return ctx, nil

}

type Jwks struct {
	Keys []JSONWebKeys `json:"keys"`
}

type JSONWebKeys struct {
	Kty string   `json:"kty"`
	Kid string   `json:"kid"`
	Use string   `json:"use"`
	N   string   `json:"n"`
	E   string   `json:"e"`
	X5c []string `json:"x5c"`
}

func getPemCert(token *jwt.Token) (interface{}, error) {

	wellKnownJWK := GetEnv(envOauth2WellKnownJwk, "")

	var jwkKeyBytes []byte
	if strings.HasPrefix(wellKnownJWK, "http") {

		resp, err := http.Get(wellKnownJWK)

		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		jwkKeyBytes, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		wellKnownJWK = string(jwkKeyBytes)
	}

	if wellKnownJWK == "" {
		return nil, errors.New("web key is missing data")
	}

	var jwks = Jwks{}
	err := json.NewDecoder(strings.NewReader(wellKnownJWK)).Decode(&jwks)

	if err != nil {
		return nil, err
	}

	for k, val := range jwks.Keys {
		if token.Header["kid"] == jwks.Keys[k].Kid {

			var exponent []byte
			if exponent, err = base64.RawURLEncoding.DecodeString(val.E); err != nil {
				return nil, err
			}

			// Decode the modulus from Base64.
			var modulus []byte
			if modulus, err = base64.RawURLEncoding.DecodeString(val.N); err != nil {
				return nil, err
			}

			// Create the RSA public key.
			publicKey := &rsa.PublicKey{}

			// Turn the exponent into an integer.
			//
			// According to RFC 7517, these numbers are in big-endian format.
			// https://tools.ietf.org/html/rfc7517#appendix-A.1
			publicKey.E = int(big.NewInt(0).SetBytes(exponent).Uint64())

			// Turn the modulus into a *big.Int.
			publicKey.N = big.NewInt(0).SetBytes(modulus)

			return publicKey, nil
		}
	}

	return nil, errors.New("unable to find appropriate key")
}

// AuthenticationMiddleware Simple http middleware function
// to verify and extract authentication data supplied in a jwt as authorization bearer token
func AuthenticationMiddleware(next http.Handler, audience string, issuer string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorizationHeader := r.Header.Get("Authorization")

		if authorizationHeader == "" || !strings.HasPrefix(authorizationHeader, "Bearer ") {
			http.Error(w, "An authorization header is required", http.StatusForbidden)
			return
		}

		extractedJwtToken := strings.Split(authorizationHeader, "Bearer ")

		if len(extractedJwtToken) != 2 {
			http.Error(w, "Malformed Authorization header", http.StatusBadRequest)
			return
		}

		jwtToken := strings.TrimSpace(extractedJwtToken[1])

		ctx := r.Context()
		ctx, err := authenticate(ctx, jwtToken, audience, issuer)

		if err != nil {
			log.Printf(" AuthenticationMiddleware -- could not authenticate token : [%s]  due to error : %+v", jwtToken, err)
			http.Error(w, "Authorization header is invalid", http.StatusUnauthorized)
			return
		}

		r = r.WithContext(ctx)

		next.ServeHTTP(w, r)
	})
}

func grpcJwtTokenExtractor(ctx context.Context) (string, error) {
	requestMetadata, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return "", status.Error(codes.Unauthenticated,"no metadata was saved in context before")
	}

	vv, ok := requestMetadata["authorization"]
	if !ok {
		return "", status.Error(codes.Unauthenticated,"no authorization key found in request metadata")
	}

	authorizationHeader := vv[0]

	if authorizationHeader == "" || !strings.HasPrefix(authorizationHeader, "bearer ") {
		return "", status.Error(codes.Unauthenticated,"an authorization header is required")
	}

	extractedJwtToken := strings.Split(authorizationHeader, "bearer ")

	if len(extractedJwtToken) != 2 {
		return "", status.Error(codes.Unauthenticated,"malformed Authorization header")
	}

	return strings.TrimSpace(extractedJwtToken[1]), nil
}

// UnaryAuthInterceptor Simple grpc interceptor to extract the jwt supplied via authorization bearer token and verify the authentication claims in the token
func UnaryAuthInterceptor(audience string, issuer string) grpc.UnaryServerInterceptor {

	return func(ctx context.Context, req interface{},
		info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {

		jwtToken, err := grpcJwtTokenExtractor(ctx)
		if err != nil {
			return nil, err
		}

		ctx, err = authenticate(ctx, jwtToken, audience, issuer)
		if err != nil {
			log.Printf(" UnaryAuthInterceptor -- could not authenticate token : [%s]  due to error : %+v", jwtToken, err)
			return nil, status.Error(codes.Unauthenticated, err.Error())
		}
		return handler(ctx, req)
	}
}

// serverStreamWrapper simple wrapper method that stores auth claims for the server stream context
type serverStreamWrapper struct {
	authClaim *AuthenticationClaims
	grpc.ServerStream
}

// Context convert the stream wrappers claims to be contained in the streams context
func (s *serverStreamWrapper) Context() context.Context {
	return s.authClaim.ClaimsToContext(s.Context())
}

// StreamAuthInterceptor An authentication claims extractor that will always verify the information flowing in the streams as true jwt claims
func StreamAuthInterceptor(audience string, issuer string) grpc.StreamServerInterceptor {
	return func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {

		localServerStream := ss
		authClaim := ClaimsFromContext(localServerStream.Context())
		if authClaim == nil {
			ctx := ss.Context()
			jwtToken, err := grpcJwtTokenExtractor(ctx)
			if err != nil {
				return err
			}

			ctx, err = authenticate(ctx, jwtToken, audience, issuer)
			if err != nil {
				log.Printf(" StreamAuthInterceptor -- could not authenticate token : [%s]  due to error : %+v", jwtToken, err)
				return status.Error(codes.Unauthenticated, err.Error())
			}

			authClaim = ClaimsFromContext(ctx)

			localServerStream = &serverStreamWrapper{authClaim, ss}
		}
		return handler(srv, localServerStream)
	}
}
