package integrations

import (
	"errors"
	"os"
	"strings"
)

func ValidateExtensions(providers map[string]*Provider, writes bool) error {
	hooks, err := requiredFeature("PROVIDER_WEBHOOKS_ENABLED")
	if err != nil {
		return err
	}
	calendar, err := requiredFeature("PROVIDER_CALENDAR_WRITES_ENABLED")
	if err != nil {
		return err
	}
	if hooks {
		base := strings.TrimRight(os.Getenv("PROVIDER_WEBHOOK_PUBLIC_URL"), "/")
		if providers["clio-manage"] == nil || endpoint(base) != nil {
			return errors.New("webhooks require configured Clio and callback endpoint")
		}
	}
	if calendar && (!writes || len(providers) == 0) {
		return errors.New("calendar writes require enabled provider writes")
	}
	return nil
}
