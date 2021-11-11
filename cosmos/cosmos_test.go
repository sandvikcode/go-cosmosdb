package cosmos

import (
	"context"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/http/httptest"
	"testing"

	"encoding/json"

	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
	"github.com/vippsas/go-cosmosdb/cosmosapi"
)

//
// Our test model
//

type MyModel struct {
	BaseModel
	Model       string `json:"model" cosmosmodel:"MyModel/1"`
	UserId      string `json:"userId"`      // partition key
	X           int    `json:"x"`           // data
	SetByPrePut string `json:"setByPrePut"` // set by pre-put hook

	XPlusOne       int `json:"-"` // computed field set by post-get hook
	PostGetCounter int // Incremented by post-get hook
}

func (e *MyModel) PrePut(txn *Transaction) error {
	e.SetByPrePut = "set by pre-put, checked in mock"
	return nil
}

func (e *MyModel) PostGet(txn *Transaction) error {
	e.XPlusOne = e.X + 1
	e.PostGetCounter += 1
	return nil
}

//
// Our mock of Cosmos DB -- this mocks the interface provided by cosmosapi package
//

type mockCosmos struct {
	Client
	ReturnX         int
	ReturnEmptyId   bool
	ReturnUserId    string
	ReturnEtag      string
	ReturnSession   string
	ReturnError     error
	GotId           string
	GotPartitionKey interface{}
	GotMethod       string
	GotUpsert       bool
	GotX            int
	GotSession      string
}

func (mock *mockCosmos) reset() {
	*mock = mockCosmos{}
}

func (mock *mockCosmos) GetDocument(ctx context.Context,
	dbName, colName, id string, ops cosmosapi.GetDocumentOptions, out interface{}) (cosmosapi.DocumentResponse, error) {

	mock.GotId = id
	mock.GotMethod = "get"
	mock.GotSession = ops.SessionToken

	t := out.(*MyModel)
	t.X = mock.ReturnX
	t.BaseModel.Etag = mock.ReturnEtag
	if mock.ReturnEmptyId {
		t.BaseModel.Id = ""
	} else {
		t.BaseModel.Id = id
	}
	t.UserId = mock.ReturnUserId
	return cosmosapi.DocumentResponse{SessionToken: mock.ReturnSession}, mock.ReturnError
}

func (mock *mockCosmos) CreateDocument(ctx context.Context,
	dbName, colName string, id *string, doc interface{}, ops cosmosapi.CreateDocumentOptions) (*cosmosapi.Resource, cosmosapi.DocumentResponse, error) {
	t := doc.(*MyModel)
	mock.GotMethod = "create"
	mock.GotPartitionKey = ops.PartitionKeyValue
	mock.GotId = t.Id
	mock.GotX = t.X
	mock.GotUpsert = ops.IsUpsert

	if t.SetByPrePut != "set by pre-put, checked in mock" {
		panic(errors.New("assertion failed"))
	}

	newBase := cosmosapi.Resource{
		Id:   t.Id,
		Etag: mock.ReturnEtag,
	}
	return &newBase, cosmosapi.DocumentResponse{SessionToken: mock.ReturnSession}, mock.ReturnError
}

func (mock *mockCosmos) ReplaceDocument(ctx context.Context,
	dbName, colName, id string, doc interface{}, ops cosmosapi.ReplaceDocumentOptions) (*cosmosapi.Resource, cosmosapi.DocumentResponse, error) {
	t := doc.(*MyModel)
	mock.GotMethod = "replace"
	mock.GotPartitionKey = ops.PartitionKeyValue
	mock.GotId = t.Id
	mock.GotX = t.X

	if t.SetByPrePut != "set by pre-put, checked in mock" {
		panic(errors.New("assertion failed"))
	}

	newBase := cosmosapi.Resource{
		Id:   t.Id,
		Etag: mock.ReturnEtag,
	}
	return &newBase, cosmosapi.DocumentResponse{SessionToken: mock.ReturnSession}, mock.ReturnError
}

func (mock *mockCosmos) ListDocuments(
	ctx context.Context,
	databaseName, collectionName string,
	options *cosmosapi.ListDocumentsOptions,
	documentList interface{},
) (response cosmosapi.ListDocumentsResponse, err error) {
	panic("implement me")
}

func (mock *mockCosmos) GetPartitionKeyRanges(
	ctx context.Context,
	databaseName, collectionName string,
	options *cosmosapi.GetPartitionKeyRangesOptions,
) (response cosmosapi.GetPartitionKeyRangesResponse, err error) {
	panic("implement me")
}

type mockCosmosNotFound struct {
	mockCosmos
}

func (mock *mockCosmosNotFound) GetDocument(ctx context.Context,
	dbName, colName, id string, ops cosmosapi.GetDocumentOptions, out interface{}) (cosmosapi.DocumentResponse, error) {
	return cosmosapi.DocumentResponse{}, cosmosapi.ErrNotFound
}

//
// Tests
//

func TestGetEntityInfo(t *testing.T) {
	c := Collection{
		Client:       &mockCosmosNotFound{},
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}
	e := MyModel{BaseModel: BaseModel{Id: "id1"}, UserId: "Alice"}
	res, pkey := c.GetEntityInfo(&e)
	require.Equal(t, "id1", res.Id)
	require.Equal(t, "Alice", pkey)
}

func TestCheckModel(t *testing.T) {
	e := MyModel{Model: "MyModel/1"}
	require.Equal(t, "MyModel/1", CheckModel(&e))
}

func TestCollectionStaleGet(t *testing.T) {
	c := Collection{
		Client:       &mockCosmosNotFound{},
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	var target MyModel
	target.X = 3
	target.Etag = "some-e-tag"
	err := c.StaleGetExisting("foo", "foo", &target)
	// StaleGetExisting: target not modified, returns not found error
	require.Equal(t, cosmosapi.ErrNotFound, errors.Cause(err))
	require.Equal(t, 3, target.X)

	// StaleGet: target zeroed, returns nil
	err = c.StaleGet("foo", "foo", &target)
	require.NoError(t, err)
	require.Equal(t, 0, target.X)
	require.Equal(t, "", target.Etag)
}

func TestCollectionRacingPut(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	entity := MyModel{
		BaseModel: BaseModel{
			Id: "id1",
		},
		X:      1,
		UserId: "alice",
	}

	require.NoError(t, c.RacingPut(&entity))
	require.Equal(t, mockCosmos{
		GotId:           "id1",
		GotPartitionKey: "alice",
		GotMethod:       "create",
		GotUpsert:       true,
		GotX:            1,
	}, mock)

	entity.Etag = "has an etag"

	// Should not affect RacingPut at all, it just does upserts..
	require.NoError(t, c.RacingPut(&entity))
	require.Equal(t, mockCosmos{
		GotId:           "id1",
		GotPartitionKey: "alice",
		GotMethod:       "create",
		GotUpsert:       true,
		GotX:            1,
	}, mock)

}

func TestTransactionCacheHappyDay(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	checkCachedEtag := func(expect string) {
		s := struct {
			Etag string `json:"_etag"`
		}{}
		key, err := newUniqueKey("partitionvalue", "idvalue")
		require.NoError(t, err)
		json.Unmarshal([]byte(session.state.entityCache[key]), &s)
		require.Equal(t, expect, s.Etag)
	}

	var entity MyModel // in production code this should be declared inside closure, but want more control in this test

	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		entity.X = -20
		mock.ReturnError = cosmosapi.ErrNotFound
		require.Equal(t, 0, len(session.state.entityCache))
		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))
		require.Equal(t, "get", mock.GotMethod)
		// due to ErrNotFound, the Get() should zero-initialize to wipe the -20
		require.Equal(t, 0, entity.X)
		require.Equal(t, 1, entity.XPlusOne) // PostGetHook called

		require.Equal(t, "idvalue", mock.GotId)
		entity.X = 42
		mock.reset()
		txn.Put(&entity)
		// *not* put yet, so mock not called yet, and not in cache
		require.Equal(t, "", mock.GotMethod)
		require.Equal(t, 1, len(session.state.entityCache))
		checkCachedEtag("")
		mock.ReturnEtag = "etag-1" // Etag returned by mock on commit; this needs to find its way into cache
		mock.ReturnSession = "session-token-1"
		return nil
	}))
	// now after exiting closure the X=42-entity was put
	// also there was a create, not a replace, because entity.Etag was empty
	require.Equal(t, "create", mock.GotMethod)
	checkCachedEtag("etag-1")

	// Session token should be set from the create call
	require.Equal(t, "session-token-1", session.Token())

	// entity outside of scope should have updated etag (this should typically not be used by code,
	// but by writing this test it is in the contract as an edge case)
	require.Equal(t, "etag-1", entity.Etag)
	// Modify entity here just to make sure it doesn't reflect what is served by cache.
	entity.X = -10

	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		mock.reset()
		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))
		// Get() above hit cache, so mock was not called
		require.Equal(t, "", mock.GotMethod)
		require.Equal(t, 42, entity.X) // i.e., not the -10 value from above
		entity.X = 43
		txn.Put(&entity)
		mock.ReturnEtag = "etag-2"
		mock.ReturnSession = "session-token-2"
		return nil
	}))
	require.Equal(t, "replace", mock.GotMethod) // this time mock returned an etag on Get(), so we got a replace
	checkCachedEtag("etag-2")

	// Session token should be set from the create call
	require.Equal(t, "session-token-2", session.Token())
}

func TestCachedGet(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()
	var entity MyModel

	resetMock := func(x int) {
		mock.reset()
		mock.ReturnEtag = "etag-1"
		mock.ReturnSession = "session"
		mock.ReturnX = x
		mock.ReturnUserId = "partitionvalue"
	}

	resetMock(42)
	require.NoError(t, session.Get("partitionvalue", "idvalue", &entity))
	require.Equal(t, "get", mock.GotMethod)
	// due to ErrNotFound, the Get() should zero-initialize to wipe the -20
	require.Equal(t, 42, entity.X)
	require.Equal(t, 43, entity.XPlusOne) // PostGetHook called
	require.Equal(t, 1, entity.PostGetCounter)
	require.Equal(t, "idvalue", mock.GotId)

	resetMock(0)
	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		mock.reset()
		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))
		// Get() above hit cache, so mock was not called
		require.Equal(t, "", mock.GotMethod)
		require.Equal(t, 42, entity.X) // not the 0 value that we've set in the mock now
		require.Equal(t, 1, entity.PostGetCounter)
		entity.X = 43
		entity.UserId = "partitionvalue"
		mock.ReturnEtag = "foobar"
		txn.Put(&entity)
		return nil
	}))

	// Check that the above Put() overwrites the cache
	resetMock(43)
	require.NoError(t, session.Get("partitionvalue", "idvalue", &entity))
	require.Equal(t, "", mock.GotMethod)
	require.Equal(t, 2, entity.PostGetCounter)
	require.Equal(t, 43, entity.X) // not the 0 value that we've set in the mock now
}

func TestTransactionCollisionAndSessionTracking(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	attempt := 0

	require.NoError(t, session.WithRetries(3).WithContext(context.Background()).Transaction(func(txn *Transaction) error {
		var entity MyModel
		mock.reset()
		mock.ReturnError = cosmosapi.ErrNotFound

		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))
		require.Equal(t, "get", mock.GotMethod)

		if attempt == 0 {
			require.Equal(t, "", mock.GotSession)
			mock.ReturnSession = "after-0"
			mock.ReturnError = cosmosapi.ErrPreconditionFailed
		} else if attempt == 1 {
			require.Equal(t, "after-0", mock.GotSession)
			mock.ReturnSession = "after-1"
			mock.ReturnError = cosmosapi.ErrPreconditionFailed
		} else if attempt == 2 {
			require.Equal(t, "after-1", mock.GotSession)
			mock.ReturnSession = "after-2"
			mock.ReturnError = nil
		}
		attempt++

		txn.Put(&entity)
		return nil
	}))

	require.Equal(t, 3, attempt)
	require.Equal(t, "after-2", session.Token())
}

func TestTransactionGetExisting(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	require.NoError(t, session.WithRetries(3).WithContext(context.Background()).Transaction(func(txn *Transaction) error {
		var entity MyModel

		mock.ReturnEtag = "etag-1"
		mock.ReturnError = nil
		mock.ReturnUserId = "partitionvalue"
		mock.ReturnX = 42
		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))
		require.False(t, entity.IsNew())
		require.Equal(t, "get", mock.GotMethod)
		require.Equal(t, 42, entity.X)
		require.Equal(t, "partitionvalue", entity.UserId)
		require.Equal(t, 43, entity.XPlusOne) // PostGetHook called
		return nil
	}))
}

func TestTransactionNonExisting(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	mock.ReturnError = cosmosapi.ErrNotFound
	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		var entity MyModel
		require.NoError(t, txn.Get("partitionValue", "idvalue", &entity))
		require.True(t, entity.IsNew())
		require.Equal(t, "partitionValue", entity.UserId)
		return nil
	}))
	return
}

func TestTransactionRollback(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()
	mock.ReturnUserId = "partitionvalue"

	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		var entity MyModel

		require.NoError(t, txn.Get("partitionvalue", "idvalue", &entity))

		mock.reset()
		txn.Put(&entity)
		return Rollback()
	}))

	// no api call done due to rollback
	require.Equal(t, "", mock.GotMethod)

}

func TestIdAsPartitionKey_GetEntityInfo(t *testing.T) {
	c := Collection{
		Client:       &mockCosmosNotFound{},
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "id",
	}
	e := MyModel{BaseModel: BaseModel{Id: "id1"}, UserId: "Alice"}
	res, pkey := c.GetEntityInfo(&e)
	require.Equal(t, "id1", res.Id)
	require.Equal(t, "id1", pkey)
}

func TestIdAsPartitionKey_TransactionGetExisting(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "id",
	}

	session := c.Session()

	require.NoError(t, session.WithRetries(3).WithContext(context.Background()).Transaction(func(txn *Transaction) error {
		var entity MyModel

		mock.ReturnEtag = "etag-1"
		mock.ReturnError = nil
		mock.ReturnX = 42
		require.NoError(t, txn.Get("idvalue", "idvalue", &entity))
		require.False(t, entity.IsNew())
		require.Equal(t, "get", mock.GotMethod)
		require.Equal(t, "idvalue", entity.Id)
		require.Equal(t, 42, entity.X)
		require.Equal(t, 43, entity.XPlusOne) // PostGetHook called
		return nil
	}))
}

func TestIdAsPartitionKey_TransactionNonExisting(t *testing.T) {
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	mock.ReturnError = cosmosapi.ErrNotFound

	require.NoError(t, session.Transaction(func(txn *Transaction) error {
		var entity MyModel
		require.NoError(t, txn.Get("idvalue", "idvalue", &entity))
		require.True(t, entity.IsNew())
		require.Equal(t, "idvalue", entity.Id)
		return nil
	}))
	return
}

func TestCollection_SanityChecksOnGet(t *testing.T) {
	// We have some sanity checks on the documents that we read from cosmos, checking that the id and
	// partition key value on the document is the same as the parameters passed to the get method.
	// This is mainly to protect against our own mistakes, not because we expect cosmos to return malformed data here
	// (although, you'll never know...)
	mock := mockCosmos{}
	c := Collection{
		Client:       &mock,
		DbName:       "mydb",
		Name:         "mycollection",
		PartitionKey: "userId"}

	session := c.Session()

	mock.ReturnUserId = ""
	err := session.Get("partitionvalue", "idvalue", &MyModel{})
	require.Error(t, err)
	require.Equal(t, fmt.Sprintf(fmtUnexpectedPartitionKeyValueError, "partitionvalue", ""), err.Error())
	mock.ReturnEmptyId = true
	mock.ReturnUserId = "partitionvalue"
	err = session.Get("partitionvalue", "idvalue", &MyModel{})
	require.Error(t, err)
	require.Equal(t, fmt.Sprintf(fmtUnexpectedIdError, "idvalue", ""), err.Error())
}

func TestTransaction_ErrorOnGet(t *testing.T) {
	var responseStatus int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(responseStatus)
	}))
	defer server.Close()
	errorf := func(f string, args ...interface{}) {
		pre := fmt.Sprintf("%s (%d): ", http.StatusText(responseStatus), responseStatus)
		t.Errorf(pre+f, args...)
	}
	for _, responseStatus = range []int{
		http.StatusTooManyRequests,     // We observed a bug on this code where Transaction.Get would ignore the error...
		http.StatusInternalServerError, // ... but other status codes in cosmosapi.CosmosHTTPErrors should have the same behavior
		http.StatusTeapot,              // Same for codes not in cosmosapi.CosmosHTTPErrors
	} {
		client := cosmosapi.New(server.URL, cosmosapi.Config{}, http.DefaultClient, log.New(ioutil.Discard, "", 0))
		coll := Collection{
			Client:       client,
			DbName:       "MyDb",
			Name:         "MyColl",
			PartitionKey: "id",
		}
		target := &MyModel{}
		err := coll.Session().Transaction(func(txn *Transaction) error {
			err := txn.Get("", "", target)
			if err == nil {
				errorf("Expected error on Transaction.Get")
			}
			return err
		})
		if err == nil {
			errorf("Expected transaction to return an error")
		}
	}
}

func TestTransaction_IgnoreErrorOnGetThenPut(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer server.Close()
	client := cosmosapi.New(server.URL, cosmosapi.Config{}, http.DefaultClient, log.New(ioutil.Discard, "", 0))
	coll := Collection{
		Client:       client,
		DbName:       "MyDb",
		Name:         "MyColl",
		PartitionKey: "id",
	}
	target := &MyModel{}
	err := coll.Session().Transaction(func(txn *Transaction) error {
		err := txn.Get("", "", target)
		if err == nil {
			t.Errorf("Expected an error")
		}
		txn.Put(target)
		return nil
	})
	if errors.Cause(err) != PutWithoutGetError {
		t.Errorf("Expected error %v", PutWithoutGetError)
	}
}
