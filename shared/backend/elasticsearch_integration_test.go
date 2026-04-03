//go:build integration

package backend

import (
	"os"
	"strconv"
	"testing"

	"socialai/shared/constants"
)

var testESBackend ElasticsearchBackendInterface

func TestMain(m *testing.M) {
	var err error
	testESBackend, err = InitElasticsearchBackend()
	if err != nil {
		panic("failed to init ES for integration test: " + err.Error())
	}
	os.Exit(m.Run())
}

func TestESBackend_SaveToES(t *testing.T) {
	testID := "integration-test-id-" + strconv.Itoa(os.Getpid())

	t.Cleanup(func() {
		_, _ = testESBackend.DeleteFromES(constants.USER_INDEX, testID)
	})

	err := testESBackend.SaveToES(map[string]interface{}{
		"user_id": testID,
	}, constants.USER_INDEX, testID)

	if err != nil {
		t.Errorf("Expected no error, got %v", err)
	}
}
