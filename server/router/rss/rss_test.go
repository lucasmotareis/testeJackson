package rss

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"

	"github.com/usememos/memos/internal/markdown"
	"github.com/usememos/memos/internal/profile"
	storepb "github.com/usememos/memos/proto/gen/store"
	"github.com/usememos/memos/store"
	teststore "github.com/usememos/memos/store/test"
)

type testRSSService struct {
	service *RSSService
	store   *store.Store
	echo    *echo.Echo
}

func newTestRSSService(t *testing.T) *testRSSService {
	t.Helper()

	ctx := context.Background()
	stores := teststore.NewTestingStore(ctx, t)
	t.Cleanup(func() {
		require.NoError(t, stores.Close())
	})

	service := NewRSSService(&profile.Profile{}, stores, markdown.NewService(markdown.WithTagExtension()))
	e := echo.New()
	service.RegisterRoutes(e.Group(""))

	return &testRSSService{
		service: service,
		store:   stores,
		echo:    e,
	}
}

func (ts *testRSSService) createUser(t *testing.T, username string) *store.User {
	t.Helper()

	user, err := ts.store.CreateUser(context.Background(), &store.User{
		Username: username,
		Role:     store.RoleUser,
		Email:    username + "@example.com",
		Nickname: username + " nickname",
	})
	require.NoError(t, err)
	return user
}

func (ts *testRSSService) createMemo(t *testing.T, user *store.User, uid string, visibility store.Visibility, content string) *store.Memo {
	t.Helper()

	memo, err := ts.store.CreateMemo(context.Background(), &store.Memo{
		UID:        uid,
		CreatorID:  user.ID,
		Content:    content,
		Visibility: visibility,
		CreatedTs:  time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC).Unix(),
		UpdatedTs:  time.Date(2024, 1, 3, 3, 4, 5, 0, time.UTC).Unix(),
	})
	require.NoError(t, err)
	return memo
}

func (ts *testRSSService) archiveMemo(t *testing.T, memo *store.Memo) {
	t.Helper()

	archived := store.Archived
	require.NoError(t, ts.store.UpdateMemo(context.Background(), &store.UpdateMemo{
		ID:        memo.ID,
		RowStatus: &archived,
	}))
}

func (ts *testRSSService) createAttachment(t *testing.T, user *store.User, memo *store.Memo, uid, filename string, storageType storepb.AttachmentStorageType, reference string) {
	t.Helper()

	_, err := ts.store.CreateAttachment(context.Background(), &store.Attachment{
		UID:         uid,
		CreatorID:   user.ID,
		Filename:    filename,
		Type:        "text/plain",
		Size:        123,
		StorageType: storageType,
		Reference:   reference,
		MemoID:      &memo.ID,
	})
	require.NoError(t, err)
}

func (ts *testRSSService) request(path string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Host = "notes.example.com"
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	rec := httptest.NewRecorder()
	ts.echo.ServeHTTP(rec, req)
	return rec
}

func TestExploreRSSIncludesOnlyPublicNormalMemos(t *testing.T) {
	ts := newTestRSSService(t)
	owner := ts.createUser(t, "owner")

	publicMemo := ts.createMemo(t, owner, "public-memo", store.Public, "# Public RSS memo\nHello **RSS**")
	ts.createMemo(t, owner, "private-memo", store.Private, "Private memo")
	ts.createMemo(t, owner, "protected-memo", store.Protected, "Protected memo")
	archivedPublicMemo := ts.createMemo(t, owner, "archived-public-memo", store.Public, "Archived memo")
	ts.archiveMemo(t, archivedPublicMemo)

	rec := ts.request("/explore/rss.xml", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "application/rss+xml")
	require.NotEmpty(t, rec.Header().Get("ETag"))
	require.Contains(t, rec.Header().Get("Cache-Control"), "public")
	require.NotEmpty(t, rec.Header().Get("Last-Modified"))

	body := rec.Body.String()
	require.Contains(t, body, "Public RSS memo")
	require.Contains(t, body, "http://notes.example.com/memos/"+publicMemo.UID)
	require.True(t,
		strings.Contains(body, "&lt;strong&gt;RSS&lt;/strong&gt;") || strings.Contains(body, "<strong>RSS</strong>"),
		"expected rendered markdown strong tag in RSS body: %s", body)
	require.NotContains(t, body, "Private memo")
	require.NotContains(t, body, "Protected memo")
	require.NotContains(t, body, "Archived memo")
}

func TestUserRSSScopesFeedToRequestedUser(t *testing.T) {
	ts := newTestRSSService(t)
	alice := ts.createUser(t, "alice")
	bob := ts.createUser(t, "bob")

	aliceMemo := ts.createMemo(t, alice, "alice-public", store.Public, "Alice public memo")
	ts.createMemo(t, alice, "alice-private", store.Private, "Alice private memo")
	ts.createMemo(t, bob, "bob-public", store.Public, "Bob public memo")

	rec := ts.request("/u/alice/rss.xml", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "Alice public memo")
	require.Contains(t, body, "http://notes.example.com/memos/"+aliceMemo.UID)
	require.Contains(t, body, "alice nickname")
	require.NotContains(t, body, "Alice private memo")
	require.NotContains(t, body, "Bob public memo")
}

func TestUserRSSReturnsNotFoundForMissingUser(t *testing.T) {
	ts := newTestRSSService(t)

	rec := ts.request("/u/missing/rss.xml", nil)

	require.Equal(t, http.StatusNotFound, rec.Code)
}

func TestRSSConditionalRequestUsesCachedETag(t *testing.T) {
	ts := newTestRSSService(t)
	owner := ts.createUser(t, "owner")
	ts.createMemo(t, owner, "cached-memo", store.Public, "Cached memo")

	first := ts.request("/explore/rss.xml", nil)
	require.Equal(t, http.StatusOK, first.Code)
	etag := first.Header().Get("ETag")
	require.NotEmpty(t, etag)

	second := ts.request("/explore/rss.xml", map[string]string{"If-None-Match": etag})

	require.Equal(t, http.StatusNotModified, second.Code)
	require.Empty(t, strings.TrimSpace(second.Body.String()))
}

func TestRSSUsesCustomProfileForFeedHeading(t *testing.T) {
	ts := newTestRSSService(t)
	owner := ts.createUser(t, "owner")
	ts.createMemo(t, owner, "custom-profile-memo", store.Public, "Custom profile memo")
	_, err := ts.store.UpsertInstanceSetting(context.Background(), &storepb.InstanceSetting{
		Key: storepb.InstanceSettingKey_GENERAL,
		Value: &storepb.InstanceSetting_GeneralSetting{
			GeneralSetting: &storepb.InstanceGeneralSetting{
				CustomProfile: &storepb.InstanceCustomProfile{
					Title:       "Team Notes",
					Description: "Public team notes",
				},
			},
		},
	})
	require.NoError(t, err)

	rec := ts.request("/explore/rss.xml", nil)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, "<title>Team Notes</title>")
	require.Contains(t, body, "<description>Public team notes</description>")
}

func TestRSSEnclosureUsesAttachmentStorageReferenceRules(t *testing.T) {
	t.Run("local attachment uses file server URL", func(t *testing.T) {
		ts := newTestRSSService(t)
		owner := ts.createUser(t, "owner")
		memo := ts.createMemo(t, owner, "local-attachment-memo", store.Public, "Local attachment memo")
		ts.createAttachment(t, owner, memo, "local-file", "note.txt", storepb.AttachmentStorageType_LOCAL, "assets/note.txt")

		rec := ts.request("/explore/rss.xml", nil)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `url="http://notes.example.com/file/attachments/local-file/note.txt"`)
		require.Contains(t, rec.Body.String(), `length="123"`)
		require.Contains(t, rec.Body.String(), `type="text/plain"`)
	})

	t.Run("external attachment uses reference URL", func(t *testing.T) {
		ts := newTestRSSService(t)
		owner := ts.createUser(t, "owner")
		memo := ts.createMemo(t, owner, "external-attachment-memo", store.Public, "External attachment memo")
		ts.createAttachment(t, owner, memo, "external-file", "remote.txt", storepb.AttachmentStorageType_EXTERNAL, "https://cdn.example.com/remote.txt")

		rec := ts.request("/explore/rss.xml", nil)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `url="https://cdn.example.com/remote.txt"`)
		require.NotContains(t, rec.Body.String(), "/file/attachments/external-file/remote.txt")
	})

	t.Run("s3 attachment uses reference URL", func(t *testing.T) {
		ts := newTestRSSService(t)
		owner := ts.createUser(t, "owner")
		memo := ts.createMemo(t, owner, "s3-attachment-memo", store.Public, "S3 attachment memo")
		ts.createAttachment(t, owner, memo, "s3-file", "object.txt", storepb.AttachmentStorageType_S3, "https://bucket.example.com/object.txt")

		rec := ts.request("/explore/rss.xml", nil)

		require.Equal(t, http.StatusOK, rec.Code)
		require.Contains(t, rec.Body.String(), `url="https://bucket.example.com/object.txt"`)
		require.NotContains(t, rec.Body.String(), "/file/attachments/s3-file/object.txt")
	})

	t.Run("only first attachment is emitted as enclosure", func(t *testing.T) {
		ts := newTestRSSService(t)
		owner := ts.createUser(t, "owner")
		memo := ts.createMemo(t, owner, "multi-attachment-memo", store.Public, "Multi attachment memo")
		ts.createAttachment(t, owner, memo, "first-file", "first.txt", storepb.AttachmentStorageType_LOCAL, "assets/first.txt")
		ts.createAttachment(t, owner, memo, "second-file", "second.txt", storepb.AttachmentStorageType_LOCAL, "assets/second.txt")

		rec := ts.request("/explore/rss.xml", nil)

		require.Equal(t, http.StatusOK, rec.Code)
		body := rec.Body.String()
		require.Equal(t, 1, strings.Count(body, "<enclosure"))
	})
}

func TestGenerateItemTitle(t *testing.T) {
	service := &RSSService{}

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "plain first line",
			content: "Plain title\nsecond line",
			want:    "Plain title",
		},
		{
			name:    "markdown heading marker is removed",
			content: "### Heading title\nbody",
			want:    "Heading title",
		},
		{
			name:    "empty title falls back to memo",
			content: "   \nbody",
			want:    "Memo",
		},
		{
			name:    "long title truncates at a word boundary",
			content: "This title is deliberately longer than one hundred characters so the RSS service can truncate it without cutting words apart",
			want:    "This title is deliberately longer than one hundred characters so the RSS service can truncate it...",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, service.generateItemTitle(tt.content))
		})
	}
}
