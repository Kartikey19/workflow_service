package mongox

import (
	"encoding/json"
	"testing"
)

func TestMongoSecurityValidation(t *testing.T) {
	for _, uri := range []string{"mongodb://localhost/?tls=false", "mongodb://localhost/?ssl=false", "mongodb://localhost/?tlsInsecure=true", "mongodb://localhost/?tlsAllowInvalidHostnames=true", "mongodb://localhost/?tlsCAFile=/etc/file", "https://localhost"} {
		if err := Validate(Config{URI: uri, Database: "analytics"}); err == nil {
			t.Errorf("accepted insecure or invalid URI %q", uri)
		}
	}
	for _, filter := range []string{`{"$where":"return true"}`, `{"$or":[{"x":1},{"$where":"return true"}]}`, `{"$expr":{"$function":{"body":"return true"}}}`} {
		if err := safeFilter(json.RawMessage(filter)); err == nil {
			t.Errorf("accepted executable filter %s", filter)
		}
	}
	if err := safeFilter(json.RawMessage(`{"score":{"$gte":10},"$or":[{"name":"first"},{"name":"second"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := Validate(Config{URI: "mongodb+srv://user:password@example.com/?authSource=admin", Database: "analytics"}); err != nil {
		t.Fatal(err)
	}
}
