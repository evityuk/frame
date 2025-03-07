package frame

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
)


const envOauth2ServiceClientSecret = "OAUTH2_SERVICE_CLIENT_SECRET"
const envOauth2ServiceAdminUri = "OAUTH2_SERVICE_ADMIN_URI"
const envOauth2Audience = "OAUTH2_SERVICE_AUDIENCE"


func (s *Service) registerForJwt(ctx context.Context) error {

	host := GetEnv(envOauth2ServiceAdminUri, "")
	if host == "" {
		return nil
	}
	clientSecret := GetEnv(envOauth2ServiceClientSecret, "")
	if clientSecret == "" {
		return nil
	}

	audienceList := strings.Split(GetEnv(envOauth2Audience, ""), ",")

	oauth2AdminUri := fmt.Sprintf("%s%s", host, "/clients")
	oauth2AdminIDUri := fmt.Sprintf("%s/%s", oauth2AdminUri, s.name)

	status, result, err := s.InvokeRestService(ctx, http.MethodGet, oauth2AdminIDUri, make(map[string]interface{}), nil)
	if err != nil {
		return err
	}

	if status == 200 {
		return nil
	}

	payload := map[string]interface{}{
		"client_id":     s.name,
		"client_name":   s.name,
		"client_secret": clientSecret,
		"grant_types":   []string{"client_credentials"},
		"metadata":      map[string]string{"cc_bot": "true"},
		"aud":           audienceList,
	}

	status, result, err = s.InvokeRestService(ctx, http.MethodPost, oauth2AdminUri, payload, nil)
	if err != nil {
		return err
	}

	if status > 299 || status < 200 {
		return errors.New(fmt.Sprintf(" invalid response status %d had message %s", status, string(result)))
	}
	return nil
}

