//go:build integration

package backend

import (
	"os"
	"strconv"
	"testing"
	"time"

	"socialai/shared/constants"

	"github.com/olivere/elastic/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func uniqueID(prefix string) string {
	return prefix + "-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func TestESBackend_SaveToES(t *testing.T) {
	testID := uniqueID("save")
	t.Cleanup(func() { _, _ = testESBackend.DeleteFromES(constants.USER_INDEX, testID) })

	err := testESBackend.SaveToES(map[string]interface{}{"user_id": testID}, constants.USER_INDEX, testID)
	require.NoError(t, err)
}

func TestESBackend_SaveAndRead(t *testing.T) {
	testID := uniqueID("read")
	t.Cleanup(func() { _, _ = testESBackend.DeleteFromES(constants.USER_INDEX, testID) })

	doc := map[string]interface{}{"user_id": testID, "username": "integration_user"}
	require.NoError(t, testESBackend.SaveToES(doc, constants.USER_INDEX, testID))

	// ES is near-real-time; wait briefly for the index to refresh.
	time.Sleep(1500 * time.Millisecond)

	query := elastic.NewTermQuery("user_id", testID)
	result, err := testESBackend.ReadFromES(query, constants.USER_INDEX)
	require.NoError(t, err)
	assert.Equal(t, int64(1), result.TotalHits(), "should find 1 document")
}

func TestESBackend_DeleteFromES(t *testing.T) {
	testID := uniqueID("delete")

	doc := map[string]interface{}{"user_id": testID}
	require.NoError(t, testESBackend.SaveToES(doc, constants.USER_INDEX, testID))

	deleted, err := testESBackend.DeleteFromES(constants.USER_INDEX, testID)
	require.NoError(t, err)
	assert.True(t, deleted)
}

func TestESBackend_IncrementField(t *testing.T) {
	testID := uniqueID("incr")
	t.Cleanup(func() { _, _ = testESBackend.DeleteFromES(constants.POST_INDEX, testID) })

	doc := map[string]interface{}{"post_id": testID, "like_count": 0}
	require.NoError(t, testESBackend.SaveToES(doc, constants.POST_INDEX, testID))

	err := testESBackend.IncrementFieldInES(constants.POST_INDEX, testID, "like_count", 5)
	require.NoError(t, err)
}

func TestESBackend_ReadFromESWithSize(t *testing.T) {
	ids := make([]string, 3)
	for i := range ids {
		ids[i] = uniqueID("size")
		doc := map[string]interface{}{"user_id": ids[i], "username": "batch_user"}
		require.NoError(t, testESBackend.SaveToES(doc, constants.USER_INDEX, ids[i]))
	}
	t.Cleanup(func() {
		for _, id := range ids {
			_, _ = testESBackend.DeleteFromES(constants.USER_INDEX, id)
		}
	})

	time.Sleep(1500 * time.Millisecond)

	query := elastic.NewTermQuery("username", "batch_user")
	result, err := testESBackend.ReadFromESWithSize(query, constants.USER_INDEX, 2)
	require.NoError(t, err)
	assert.LessOrEqual(t, len(result.Hits.Hits), 2, "should respect size limit")
}
