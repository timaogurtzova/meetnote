package postgres_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/timaogurtzova/meetnote/internal/domain"
	"github.com/timaogurtzova/meetnote/internal/inbox"
	"github.com/timaogurtzova/meetnote/internal/postgres"
)

func TestRepositoryFullScenarioAndUserIsolation(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	require.NoError(t, postgres.Migrate(ctx, pool))
	require.NoError(t, postgres.Migrate(ctx, pool))
	var appliedMigrations int
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM goose_db_version
		WHERE version_id > 0 AND is_applied
	`).Scan(&appliedMigrations)
	require.NoError(t, err)
	assert.Equal(t, 4, appliedMigrations)
	repository, err := postgres.NewRepository(pool)
	require.NoError(t, err)

	const alice = "integration-alice"
	const bob = "integration-bob"
	if err := repository.RegisterUser(ctx, bob); err != nil {
		t.Fatal(err)
	}
	meeting, created, err := repository.CreateMeeting(ctx, alice, "test:create:1", domain.StoredFile{
		OriginalFilename: "planning.txt",
		Path:             "/test/planning.txt",
		Size:             100,
	}, testMeetingQuota())
	if err != nil {
		t.Fatalf("CreateMeeting() error: %v", err)
	}
	if !created {
		t.Fatal("first CreateMeeting() was reported as duplicate")
	}
	duplicate, duplicateCreated, err := repository.CreateMeeting(ctx, alice, "test:create:1", domain.StoredFile{
		OriginalFilename: "duplicate.txt",
		Path:             "/test/duplicate.txt",
	}, testMeetingQuota())
	if err != nil || duplicateCreated || duplicate.ID != meeting.ID {
		t.Fatalf("idempotent CreateMeeting() = %#v, created=%v, err=%v", duplicate, duplicateCreated, err)
	}
	var meetingCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM meetings`).Scan(&meetingCount); err != nil || meetingCount != 1 {
		t.Fatalf("meeting count after duplicate = %d, err=%v", meetingCount, err)
	}
	if meeting.Status != domain.StatusCreated {
		t.Fatalf("initial status = %s", meeting.Status)
	}
	var storedFileSize int64
	if err := pool.QueryRow(ctx, `SELECT file_size FROM meetings WHERE id = $1`, meeting.ID).Scan(&storedFileSize); err != nil || storedFileSize != 100 {
		t.Fatalf("stored file size = %d, err=%v", storedFileSize, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE meetings SET file_size = -1 WHERE id = $1`, meeting.ID); err == nil {
		t.Fatal("database accepted a negative file size")
	}
	var storedPaths []string
	for storedPath, pathErr := range repository.ListStoredPaths(ctx) {
		require.NoError(t, pathErr)
		storedPaths = append(storedPaths, storedPath)
	}
	assert.Equal(t, []string{"/test/planning.txt"}, storedPaths)

	if _, err := repository.GetMeeting(ctx, bob, meeting.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bob GetMeeting() error = %v, want not found", err)
	}
	bobMeetings, err := repository.ListMeetings(ctx, bob, 10)
	if err != nil || len(bobMeetings) != 0 {
		t.Fatalf("bob ListMeetings() = %#v, %v", bobMeetings, err)
	}

	task, found, err := repository.ClaimNextTask(ctx, "integration-worker", time.Minute)
	if err != nil || !found || task.MeetingID != meeting.ID {
		t.Fatalf("ClaimNextTask() = %#v, %v, %v", task, found, err)
	}
	const transcript = "Анна отвечает за релиз. Релиз запланирован на пятницу."
	if err := repository.SaveTranscription(ctx, task, transcript); err != nil {
		t.Fatalf("SaveTranscription() error: %v", err)
	}
	if err := repository.SaveSummary(ctx, task, "Релиз в пятницу, ответственная Анна."); err != nil {
		t.Fatalf("SaveSummary() error: %v", err)
	}
	if err := repository.CompleteTask(ctx, task); err != nil {
		t.Fatalf("CompleteTask() error: %v", err)
	}
	rows, err := pool.Query(ctx, `
		SELECT h.to_status
		FROM task_status_history h
		JOIN processing_tasks p ON p.id = h.task_id
		WHERE p.meeting_id = $1
		ORDER BY h.changed_at, h.id
	`, meeting.ID)
	if err != nil {
		t.Fatal(err)
	}
	var transitions []string
	for rows.Next() {
		var status string
		if err := rows.Scan(&status); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		transitions = append(transitions, status)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		t.Fatal(err)
	}
	rows.Close()
	wantTransitions := []string{"created", "processing", "transcribed", "summarized", "completed"}
	if fmt.Sprint(transitions) != fmt.Sprint(wantTransitions) {
		t.Fatalf("status transitions = %v, want %v", transitions, wantTransitions)
	}

	gotTranscript, err := repository.GetTranscript(ctx, alice, meeting.ID)
	if err != nil || gotTranscript != transcript {
		t.Fatalf("GetTranscript() = %q, %v", gotTranscript, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE transcriptions SET content = ' ' WHERE meeting_id = $1`, meeting.ID); err == nil {
		t.Fatal("database accepted a blank transcription")
	}
	if _, err := repository.GetTranscript(ctx, bob, meeting.ID); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bob GetTranscript() error = %v, want not found", err)
	}
	result, err := repository.FindMeetings(ctx, alice, "релиз", 10)
	if err != nil || len(result) != 1 || result[0].MeetingID != meeting.ID {
		t.Fatalf("FindMeetings() = %#v, %v", result, err)
	}
	wildcardResult, err := repository.FindMeetings(ctx, alice, "%", 10)
	if err != nil || len(wildcardResult) != 0 {
		t.Fatalf("FindMeetings literal wildcard = %#v, %v", wildcardResult, err)
	}
	bobResult, err := repository.FindMeetings(ctx, bob, "релиз", 10)
	if err != nil || len(bobResult) != 0 {
		t.Fatalf("bob FindMeetings() = %#v, %v", bobResult, err)
	}
	documents, err := repository.ChatDocuments(ctx, alice, 10)
	if err != nil || len(documents) != 1 {
		t.Fatalf("ChatDocuments() = %#v, %v", documents, err)
	}
	bobDocuments, err := repository.ChatDocuments(ctx, bob, 10)
	if err != nil || len(bobDocuments) != 0 {
		t.Fatalf("bob ChatDocuments() = %#v, %v", bobDocuments, err)
	}
	storedAnswer, err := repository.SaveChat(ctx, alice, "test:chat:1", "Who?", "Anna", 2)
	if err != nil || storedAnswer != "Anna" {
		t.Fatalf("first SaveChat() = %q, %v", storedAnswer, err)
	}
	storedAnswer, err = repository.SaveChat(ctx, alice, "test:chat:1", "Different question", "Different answer", 2)
	if err != nil || storedAnswer != "Anna" {
		t.Fatalf("idempotent SaveChat() = %q, %v, want original answer", storedAnswer, err)
	}
	storedAnswer, found, err = repository.GetChatAnswer(ctx, alice, "test:chat:1")
	if err != nil || !found || storedAnswer != "Anna" {
		t.Fatalf("GetChatAnswer() = %q, found=%v, err=%v", storedAnswer, found, err)
	}
	if _, found, err := repository.GetChatAnswer(ctx, bob, "test:chat:1"); err != nil || found {
		t.Fatalf("bob GetChatAnswer() found=%v, err=%v", found, err)
	}
	if _, err := repository.SaveChat(ctx, alice, "test:chat:2", "Second?", "Second", 2); err != nil {
		t.Fatalf("second SaveChat() error: %v", err)
	}
	if _, err := repository.SaveChat(ctx, alice, "test:chat:3", "Third?", "Third", 2); err != nil {
		t.Fatalf("third SaveChat() error: %v", err)
	}
	if _, found, err := repository.GetChatAnswer(ctx, alice, "test:chat:1"); err != nil || found {
		t.Fatalf("oldest chat answer found=%v, err=%v, want pruned", found, err)
	}
	var chatRows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM chat_history`).Scan(&chatRows); err != nil || chatRows != 2 {
		t.Fatalf("chat rows after history trim = %d, err=%v", chatRows, err)
	}

	failedMeeting, _, err := repository.CreateMeeting(ctx, alice, "test:create:2", domain.StoredFile{
		OriginalFilename: "failed.txt",
		Path:             "/test/failed.txt",
	}, testMeetingQuota())
	if err != nil {
		t.Fatal(err)
	}
	failedTask, found, err := repository.ClaimNextTask(ctx, "integration-worker", time.Minute)
	if err != nil || !found || failedTask.MeetingID != failedMeeting.ID {
		t.Fatalf("claim failed task = %#v, %v, %v", failedTask, found, err)
	}
	if err := repository.FailTask(ctx, failedTask, domain.ProcessingFailure{Code: "speech_failed", Message: "safe failure"}); err != nil {
		t.Fatalf("FailTask() error: %v", err)
	}
	status, err := repository.GetMeeting(ctx, alice, failedMeeting.ID)
	if err != nil || status.Status != domain.StatusFailed || status.Error == "" {
		t.Fatalf("failed status = %#v, %v", status, err)
	}
	if err := repository.RetryMeeting(ctx, bob, "test:retry:1", failedMeeting.ID, 10); !errors.Is(err, domain.ErrNotFound) {
		t.Fatalf("bob RetryMeeting() error = %v, want not found", err)
	}
	if err := repository.RetryMeeting(ctx, alice, "test:retry:2", failedMeeting.ID, 10); err != nil {
		t.Fatalf("RetryMeeting() error: %v", err)
	}
	if err := repository.RetryMeeting(ctx, alice, "test:retry:2", failedMeeting.ID, 10); err != nil {
		t.Fatalf("idempotent RetryMeeting() error: %v", err)
	}
	status, err = repository.GetMeeting(ctx, alice, failedMeeting.ID)
	if err != nil || status.Status != domain.StatusCreated {
		t.Fatalf("retried status = %#v, %v", status, err)
	}

	var indexCount int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM pg_indexes
		WHERE schemaname = current_schema()
		  AND indexname IN (
		      'idx_meetings_user_created',
		      'idx_tasks_status_created',
		      'idx_transcriptions_fts',
		      'idx_summaries_fts',
		      'idx_tasks_expired_leases',
		      'idx_processing_user_leases_expiry'
		  )
	`).Scan(&indexCount); err != nil {
		t.Fatal(err)
	}
	if indexCount != 6 {
		t.Fatalf("important index count = %d, want 6", indexCount)
	}
}

func TestRepositoryEnforcesMeetingQuotasTransactionally(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	quota := domain.MeetingQuota{MaxMeetings: 2, MaxPending: 1, MaxStorageBytes: 10}

	first, created, err := repository.CreateMeeting(ctx, "quota-user", "quota:first", domain.StoredFile{
		OriginalFilename: "first.txt", Path: "/test/quota-first.txt", Size: 6,
	}, quota)
	if err != nil || !created {
		t.Fatalf("first CreateMeeting() = %#v, created=%v, err=%v", first, created, err)
	}
	duplicate, created, err := repository.CreateMeeting(ctx, "quota-user", "quota:first", domain.StoredFile{
		OriginalFilename: "duplicate.txt", Path: "/test/quota-duplicate.txt", Size: 10,
	}, quota)
	if err != nil || created || duplicate.ID != first.ID {
		t.Fatalf("idempotent request at quota = %#v, created=%v, err=%v", duplicate, created, err)
	}
	if _, _, err := repository.CreateMeeting(ctx, "quota-user", "quota:pending", domain.StoredFile{
		OriginalFilename: "pending.txt", Path: "/test/quota-pending.txt", Size: 1,
	}, quota); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("pending quota error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE processing_tasks SET status = 'completed' WHERE meeting_id = $1`, first.ID); err != nil {
		t.Fatal(err)
	}
	second, created, err := repository.CreateMeeting(ctx, "quota-user", "quota:second", domain.StoredFile{
		OriginalFilename: "second.txt", Path: "/test/quota-second.txt", Size: 4,
	}, quota)
	if err != nil || !created {
		t.Fatalf("second CreateMeeting() = %#v, created=%v, err=%v", second, created, err)
	}
	if _, _, err := repository.CreateMeeting(ctx, "quota-user", "quota:total", domain.StoredFile{
		OriginalFilename: "third.txt", Path: "/test/quota-third.txt", Size: 1,
	}, quota); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("total quota error = %v", err)
	}
	if _, _, err := repository.CreateMeeting(ctx, "oversize-user", "quota:oversize", domain.StoredFile{
		OriginalFilename: "large.txt", Path: "/test/quota-large.txt", Size: 11,
	}, quota); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("storage quota error = %v", err)
	}

	type createResult struct {
		created bool
		err     error
	}
	results := make(chan createResult, 2)
	start := make(chan struct{})
	for index := 0; index < 2; index++ {
		go func(index int) {
			<-start
			_, created, err := repository.CreateMeeting(ctx, "parallel-quota-user", fmt.Sprintf("parallel:%d", index), domain.StoredFile{
				OriginalFilename: fmt.Sprintf("parallel-%d.txt", index),
				Path:             fmt.Sprintf("/test/parallel-%d.txt", index),
				Size:             1,
			}, quota)
			results <- createResult{created: created, err: err}
		}(index)
	}
	close(start)
	createdCount, rejectedCount := 0, 0
	for range 2 {
		result := <-results
		switch {
		case result.err == nil && result.created:
			createdCount++
		case errors.Is(result.err, domain.ErrQuotaExceeded):
			rejectedCount++
		default:
			t.Fatalf("parallel CreateMeeting() unexpected result: created=%v err=%v", result.created, result.err)
		}
	}
	if createdCount != 1 || rejectedCount != 1 {
		t.Fatalf("parallel quota results: created=%d rejected=%d", createdCount, rejectedCount)
	}

	retryQuota := domain.MeetingQuota{MaxMeetings: 3, MaxPending: 1, MaxStorageBytes: 100}
	failed, _, err := repository.CreateMeeting(ctx, "retry-quota-user", "retry:failed", domain.StoredFile{
		OriginalFilename: "failed.txt", Path: "/test/retry-failed.txt", Size: 1,
	}, retryQuota)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE processing_tasks SET status = 'failed' WHERE meeting_id = $1`, failed.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.CreateMeeting(ctx, "retry-quota-user", "retry:pending", domain.StoredFile{
		OriginalFilename: "pending.txt", Path: "/test/retry-pending.txt", Size: 1,
	}, retryQuota); err != nil {
		t.Fatal(err)
	}
	if err := repository.RetryMeeting(ctx, "retry-quota-user", "retry:key", failed.ID, retryQuota.MaxPending); !errors.Is(err, domain.ErrQuotaExceeded) {
		t.Fatalf("retry pending quota error = %v", err)
	}

	idempotentRetry, _, err := repository.CreateMeeting(ctx, "idempotent-retry-user", "retry:idempotent", domain.StoredFile{
		OriginalFilename: "idempotent.txt", Path: "/test/retry-idempotent.txt", Size: 1,
	}, retryQuota)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE processing_tasks SET status = 'failed' WHERE meeting_id = $1`, idempotentRetry.ID); err != nil {
		t.Fatal(err)
	}
	if err := repository.RetryMeeting(ctx, "idempotent-retry-user", "retry:same-key", idempotentRetry.ID, 1); err != nil {
		t.Fatal(err)
	}
	if err := repository.RetryMeeting(ctx, "idempotent-retry-user", "retry:same-key", idempotentRetry.ID, 1); err != nil {
		t.Fatalf("idempotent retry at pending quota error: %v", err)
	}
}

func TestRepositoryLimitsOneActiveProcessingTaskPerUser(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	var aliceMeetingIDs []int64
	var bobMeetingID int64
	for index, owner := range []string{"fair-alice", "fair-alice", "fair-bob"} {
		meeting, _, err := repository.CreateMeeting(ctx, owner, fmt.Sprintf("fair:%d", index), domain.StoredFile{
			OriginalFilename: fmt.Sprintf("fair-%d.txt", index), Path: fmt.Sprintf("/test/fair-%d.txt", index), Size: 1,
		}, testMeetingQuota())
		if err != nil {
			t.Fatal(err)
		}
		if owner == "fair-alice" {
			aliceMeetingIDs = append(aliceMeetingIDs, meeting.ID)
		} else {
			bobMeetingID = meeting.ID
		}
	}
	aliceFirst, found, err := repository.ClaimNextTask(ctx, "fair-worker-1", time.Minute)
	if err != nil || !found || aliceFirst.MeetingID != aliceMeetingIDs[0] {
		t.Fatalf("first claim = %#v, found=%v, err=%v", aliceFirst, found, err)
	}
	bob, found, err := repository.ClaimNextTask(ctx, "fair-worker-2", time.Minute)
	if err != nil || !found || bob.MeetingID != bobMeetingID {
		t.Fatalf("second claim must skip busy Alice = %#v, found=%v, err=%v", bob, found, err)
	}
	if _, found, err := repository.ClaimNextTask(ctx, "fair-worker-3", time.Minute); err != nil || found {
		t.Fatalf("third claim found=%v err=%v, want no claimable task", found, err)
	}
	if err := repository.RefreshLease(ctx, aliceFirst, 2*time.Minute); err != nil {
		t.Fatalf("RefreshLease(Alice) error: %v", err)
	}
	var taskLease, userLease time.Time
	if err := pool.QueryRow(ctx, `
		SELECT p.locked_until, l.locked_until
		FROM processing_tasks p
		JOIN processing_user_leases l ON l.task_id = p.id
		WHERE p.id = $1
	`, aliceFirst.ID).Scan(&taskLease, &userLease); err != nil {
		t.Fatal(err)
	}
	if !taskLease.Equal(userLease) {
		t.Fatalf("task lease %s and user lease %s differ", taskLease, userLease)
	}
	if err := repository.FailTask(ctx, aliceFirst, domain.ProcessingFailure{Code: "test", Message: "test"}); err != nil {
		t.Fatalf("FailTask(Alice) error: %v", err)
	}
	aliceSecond, found, err := repository.ClaimNextTask(ctx, "fair-worker-3", time.Minute)
	if err != nil || !found || aliceSecond.MeetingID != aliceMeetingIDs[1] || aliceSecond.ID == aliceFirst.ID {
		t.Fatalf("Alice second claim after release = %#v, found=%v, err=%v", aliceSecond, found, err)
	}

	for index := 0; index < 8; index++ {
		if _, _, err := repository.CreateMeeting(ctx, "race-owner", fmt.Sprintf("race-owner:%d", index), domain.StoredFile{
			OriginalFilename: fmt.Sprintf("race-%d.txt", index), Path: fmt.Sprintf("/test/race-%d.txt", index), Size: 1,
		}, testMeetingQuota()); err != nil {
			t.Fatal(err)
		}
	}
	type claimResult struct {
		found bool
		err   error
	}
	claims := make(chan claimResult, 4)
	start := make(chan struct{})
	var claimers sync.WaitGroup
	for workerID := 0; workerID < 4; workerID++ {
		claimers.Add(1)
		go func(workerID int) {
			defer claimers.Done()
			<-start
			_, found, err := repository.ClaimNextTask(ctx, fmt.Sprintf("race-worker-%d", workerID), time.Minute)
			claims <- claimResult{found: found, err: err}
		}(workerID)
	}
	close(start)
	claimers.Wait()
	close(claims)
	claimed := 0
	for result := range claims {
		if result.err != nil {
			t.Fatalf("concurrent per-user claim error: %v", result.err)
		}
		if result.found {
			claimed++
		}
	}
	if claimed != 1 {
		t.Fatalf("concurrent active tasks for one user = %d, want 1", claimed)
	}
}

func TestTelegramInboxDeduplicatesAndLimitsOneInFlightUpdatePerUser(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	updates := []inbox.Item{
		{ID: 1, OwnerKey: "telegram:alice", Payload: []byte(`{"update_id":1}`)},
		{ID: 2, OwnerKey: "telegram:alice", Payload: []byte(`{"update_id":2}`)},
		{ID: 3, OwnerKey: "telegram:bob", Payload: []byte(`{"update_id":3}`)},
		{ID: 1, OwnerKey: "telegram:alice", Payload: []byte(`{"update_id":1}`)},
	}
	if err := repository.Enqueue(ctx, updates); err != nil {
		t.Fatalf("Enqueue() error: %v", err)
	}
	if offset, err := repository.NextOffset(ctx); err != nil || offset != 4 {
		t.Fatalf("NextOffset() = %d, %v", offset, err)
	}

	alice, found, err := repository.Claim(ctx, "worker-1", time.Minute)
	if err != nil || !found || alice.ID != 1 {
		t.Fatalf("first Claim() = %#v, %v, %v", alice, found, err)
	}
	bob, found, err := repository.Claim(ctx, "worker-2", time.Minute)
	if err != nil || !found || bob.ID != 3 {
		t.Fatalf("second Claim() must skip busy Alice and claim Bob: %#v, %v, %v", bob, found, err)
	}
	if _, found, err := repository.Claim(ctx, "worker-3", time.Minute); err != nil || found {
		t.Fatalf("third Claim() found=%v err=%v, want no claimable update", found, err)
	}
	if err := repository.Complete(ctx, alice); err != nil {
		t.Fatalf("Complete(Alice) error: %v", err)
	}
	aliceSecond, found, err := repository.Claim(ctx, "worker-3", time.Minute)
	if err != nil || !found || aliceSecond.ID != 2 {
		t.Fatalf("Alice second Claim() = %#v, %v, %v", aliceSecond, found, err)
	}
	terminal, err := repository.Retry(ctx, aliceSecond, "safe failure", 0, 3)
	if err != nil || terminal {
		t.Fatalf("Retry() terminal=%v err=%v", terminal, err)
	}
	aliceRetried, found, err := repository.Claim(ctx, "worker-4", time.Minute)
	if err != nil || !found || aliceRetried.ID != 2 || aliceRetried.Attempt != 2 {
		t.Fatalf("retried Claim() = %#v, %v, %v", aliceRetried, found, err)
	}
	if err := repository.Complete(ctx, aliceRetried); err != nil {
		t.Fatalf("Complete(retried Alice) error: %v", err)
	}
	if err := repository.Complete(ctx, bob); err != nil {
		t.Fatalf("Complete(Bob) error: %v", err)
	}
	var stored int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM telegram_updates`).Scan(&stored); err != nil || stored != 3 {
		t.Fatalf("stored Telegram updates = %d, err=%v", stored, err)
	}

	var sameUserUpdates []inbox.Item
	for updateID := int64(10); updateID < 20; updateID++ {
		sameUserUpdates = append(sameUserUpdates, inbox.Item{
			ID: updateID, OwnerKey: "telegram:carol", Payload: []byte(fmt.Sprintf(`{"update_id":%d}`, updateID)),
		})
	}
	if err := repository.Enqueue(ctx, sameUserUpdates); err != nil {
		t.Fatal(err)
	}
	type claimResult struct {
		update inbox.Item
		found  bool
		err    error
	}
	claims := make(chan claimResult, 4)
	start := make(chan struct{})
	var claimers sync.WaitGroup
	for workerID := 0; workerID < 4; workerID++ {
		claimers.Add(1)
		go func(id int) {
			defer claimers.Done()
			<-start
			update, found, err := repository.Claim(ctx, fmt.Sprintf("concurrent-%d", id), time.Minute)
			claims <- claimResult{update: update, found: found, err: err}
		}(workerID)
	}
	close(start)
	claimers.Wait()
	close(claims)
	claimedForCarol := 0
	var carol inbox.Item
	for result := range claims {
		if result.err != nil {
			t.Fatalf("concurrent Claim() error: %v", result.err)
		}
		if result.found {
			claimedForCarol++
			carol = result.update
		}
	}
	if claimedForCarol != 1 {
		t.Fatalf("concurrent in-flight updates for one user = %d, want 1", claimedForCarol)
	}
	if err := repository.Complete(ctx, carol); err != nil {
		t.Fatalf("complete concurrent claim: %v", err)
	}

}

func TestTelegramInboxRecoversExpiredLeaseAndPrunesCompletedUpdates(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Enqueue(ctx, []inbox.Item{{
		ID: 30, OwnerKey: "telegram:dora", Payload: []byte(`{"update_id":30}`),
	}}); err != nil {
		t.Fatal(err)
	}
	expired, found, err := repository.Claim(ctx, "expired-worker", time.Minute)
	if err != nil || !found || expired.ID != 30 {
		t.Fatalf("claim update to expire = %#v, found=%v, err=%v", expired, found, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE telegram_updates SET locked_until = now() - interval '1 second' WHERE update_id = $1`, expired.ID); err != nil {
		t.Fatalf("expire Telegram update lease: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE telegram_user_leases SET locked_until = now() - interval '1 second' WHERE update_id = $1`, expired.ID); err != nil {
		t.Fatalf("expire Telegram user lease: %v", err)
	}
	if _, err := repository.Retry(ctx, expired, "stale result", 0, 3); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale Retry() error = %v, want lease lost", err)
	}
	recovered, err := repository.RecoverExpired(ctx)
	if err != nil || recovered != 1 {
		t.Fatalf("RecoverExpired() = %d, %v", recovered, err)
	}
	reclaimed, found, err := repository.Claim(ctx, "replacement-worker", time.Minute)
	if err != nil || !found || reclaimed.ID != expired.ID || reclaimed.LeaseToken == expired.LeaseToken {
		t.Fatalf("reclaimed update = %#v, found=%v, err=%v", reclaimed, found, err)
	}
	if err := repository.Complete(ctx, reclaimed); err != nil {
		t.Fatalf("complete reclaimed update: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE telegram_updates SET updated_at = now() - interval '8 days' WHERE update_id = 30`); err != nil {
		t.Fatalf("age completed update: %v", err)
	}
	pruned, err := repository.Prune(ctx, time.Now().Add(-7*24*time.Hour))
	if err != nil || pruned != 1 {
		t.Fatalf("Prune() = %d, %v", pruned, err)
	}
}

func TestTelegramInboxDoesNotRunTwoUpdatesWhenUserLeaseIsMissing(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}
	if err := repository.Enqueue(ctx, []inbox.Item{
		{ID: 40, OwnerKey: "telegram:erin", Payload: []byte(`{"update_id":40}`)},
		{ID: 41, OwnerKey: "telegram:erin", Payload: []byte(`{"update_id":41}`)},
	}); err != nil {
		t.Fatal(err)
	}
	first, found, err := repository.Claim(ctx, "first-worker", time.Minute)
	if err != nil || !found || first.ID != 40 {
		t.Fatalf("first Claim() = %#v, found=%v, err=%v", first, found, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM telegram_user_leases WHERE update_id = $1`, first.ID); err != nil {
		t.Fatalf("delete user lease: %v", err)
	}
	if _, found, err := repository.Claim(ctx, "second-worker", time.Minute); err != nil || found {
		t.Fatalf("Claim() with missing user lease found=%v, err=%v, want no update", found, err)
	}
	if err := repository.Complete(ctx, first); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("Complete() without user lease error = %v, want lease lost", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE telegram_updates
		SET locked_until = now() - interval '1 second'
		WHERE update_id = $1
	`, first.ID); err != nil {
		t.Fatalf("expire update lease: %v", err)
	}
	if recovered, err := repository.RecoverExpired(ctx); err != nil || recovered != 1 {
		t.Fatalf("RecoverExpired() = %d, %v", recovered, err)
	}
	for _, updateID := range []int64{40, 41} {
		claimed, found, err := repository.Claim(ctx, "replacement-worker", time.Minute)
		if err != nil || !found || claimed.ID != updateID {
			t.Fatalf("replacement Claim() = %#v, found=%v, err=%v, want %d", claimed, found, err, updateID)
		}
		if err := repository.Complete(ctx, claimed); err != nil {
			t.Fatalf("Complete(%d) error: %v", updateID, err)
		}
	}
}

func TestMigrateUpgradesPreviousSchemaAndPreservesData(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)

	provider, closeProvider, err := newTestMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.UpTo(ctx, 2); err != nil {
		closeProvider()
		t.Fatalf("apply first two migrations: %v", err)
	}
	closeProvider()

	if _, err := pool.Exec(ctx, `
		INSERT INTO users(external_id) VALUES('legacy-user');
		INSERT INTO meetings(user_id, original_filename, stored_path)
		SELECT id, 'legacy.txt', '/test/legacy.txt' FROM users WHERE external_id = 'legacy-user';
		INSERT INTO processing_tasks(meeting_id, status)
		SELECT id, 'completed' FROM meetings WHERE original_filename = 'legacy.txt';
	`); err != nil {
		t.Fatalf("seed legacy meeting: %v", err)
	}

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() schema upgrade error: %v", err)
	}
	var appliedMigrations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM goose_db_version
		WHERE version_id > 0 AND is_applied
	`).Scan(&appliedMigrations); err != nil {
		t.Fatal(err)
	}
	if appliedMigrations != 4 {
		t.Fatalf("migrations after schema upgrade = %d, want 4", appliedMigrations)
	}
	var legacyFileSize int64
	if err := pool.QueryRow(ctx, `SELECT file_size FROM meetings WHERE original_filename = 'legacy.txt'`).Scan(&legacyFileSize); err != nil {
		t.Fatal(err)
	}
	const legacyTelegramFileSize = int64(20 * 1024 * 1024)
	if legacyFileSize != legacyTelegramFileSize {
		t.Fatalf("legacy file size = %d, want conservative %d", legacyFileSize, legacyTelegramFileSize)
	}
	var inboxTable string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('telegram_updates')::text`).Scan(&inboxTable); err != nil || inboxTable != "telegram_updates" {
		t.Fatalf("telegram_updates table = %q, err=%v", inboxTable, err)
	}
}

func TestMigrationsCanRollbackAndApplyAgain(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)

	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	provider, closeProvider, err := newTestMigrationProvider(pool)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 0); err != nil {
		closeProvider()
		t.Fatalf("rollback migrations: %v", err)
	}
	closeProvider()

	var usersTableMissing bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('users') IS NULL`).Scan(&usersTableMissing); err != nil {
		t.Fatal(err)
	}
	if !usersTableMissing {
		t.Fatal("users table still exists after full rollback")
	}
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("reapply migrations: %v", err)
	}
	var usersTablePresent bool
	if err := pool.QueryRow(ctx, `SELECT to_regclass('users') IS NOT NULL`).Scan(&usersTablePresent); err != nil {
		t.Fatal(err)
	}
	if !usersTablePresent {
		t.Fatal("users table is missing after repeated migration")
	}
}

func TestConcurrentMigrationsAreSerialized(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)

	start := make(chan struct{})
	migrationErrors := make(chan error, 2)
	var migrations sync.WaitGroup
	for range 2 {
		migrations.Add(1)
		go func() {
			defer migrations.Done()
			<-start
			migrationErrors <- postgres.Migrate(ctx, pool)
		}()
	}
	close(start)
	migrations.Wait()
	close(migrationErrors)
	for err := range migrationErrors {
		if err != nil {
			t.Fatalf("concurrent Migrate() error: %v", err)
		}
	}

	var appliedMigrations int
	if err := pool.QueryRow(ctx, `
		SELECT count(*)
		FROM goose_db_version
		WHERE version_id > 0 AND is_applied
	`).Scan(&appliedMigrations); err != nil {
		t.Fatal(err)
	}
	if appliedMigrations != 4 {
		t.Fatalf("applied migrations = %d, want 4", appliedMigrations)
	}
}

func TestRepositoryLeasesProtectConcurrentWorkers(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := isolatedTestPool(t, ctx, databaseURL)
	if err := postgres.Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate() error: %v", err)
	}
	repository, err := postgres.NewRepository(pool)
	if err != nil {
		t.Fatal(err)
	}

	const taskCount = 12
	for index := 0; index < taskCount; index++ {
		_, _, err := repository.CreateMeeting(ctx, fmt.Sprintf("lease-user-%d", index), fmt.Sprintf("test:lease:%d", index), domain.StoredFile{
			OriginalFilename: fmt.Sprintf("meeting-%d.txt", index),
			Path:             fmt.Sprintf("/test/meeting-%d.txt", index),
		}, testMeetingQuota())
		if err != nil {
			t.Fatalf("CreateMeeting(%d) error: %v", index, err)
		}
	}

	claimed := make(chan domain.Task, taskCount)
	claimErrors := make(chan error, 4)
	var workers sync.WaitGroup
	for workerID := 1; workerID <= 4; workerID++ {
		workers.Add(1)
		go func(id int) {
			defer workers.Done()
			for {
				task, found, err := repository.ClaimNextTask(
					ctx,
					fmt.Sprintf("replica-%d", id),
					time.Minute,
				)
				if err != nil {
					claimErrors <- err
					return
				}
				if !found {
					return
				}
				claimed <- task
			}
		}(workerID)
	}
	workers.Wait()
	close(claimed)
	close(claimErrors)
	for err := range claimErrors {
		t.Fatalf("concurrent ClaimNextTask() error: %v", err)
	}

	seen := make(map[int64]domain.Task, taskCount)
	for task := range claimed {
		if task.LeaseToken == "" {
			t.Fatalf("claimed task has no lease: %#v", task)
		}
		if _, duplicate := seen[task.ID]; duplicate {
			t.Fatalf("task %d was claimed more than once", task.ID)
		}
		seen[task.ID] = task
	}
	if len(seen) != taskCount {
		t.Fatalf("claimed %d tasks, want %d", len(seen), taskCount)
	}

	recovered, err := repository.RecoverInterrupted(ctx)
	if err != nil || recovered != 0 {
		t.Fatalf("live leases recovered = %d, error = %v", recovered, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE processing_tasks
		SET locked_until = now() - interval '1 second'
		WHERE status = 'processing'
	`); err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	recovered, err = repository.RecoverInterrupted(ctx)
	if err != nil || recovered != taskCount {
		t.Fatalf("expired leases recovered = %d, error = %v", recovered, err)
	}

	var staleTask domain.Task
	for _, task := range seen {
		staleTask = task
		break
	}
	if err := repository.SaveTranscription(ctx, staleTask, "stale result"); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale worker SaveTranscription() error = %v, want lease lost", err)
	}
	reclaimed, found, err := repository.ClaimNextTask(ctx, "replacement-replica", time.Minute)
	if err != nil || !found {
		t.Fatalf("reclaim expired task = %#v, %v, %v", reclaimed, found, err)
	}
	if reclaimed.LeaseToken == staleTask.LeaseToken {
		t.Fatal("reclaimed task reused a stale lease token")
	}
	if err := repository.RefreshLease(ctx, staleTask, time.Minute); !errors.Is(err, domain.ErrLeaseLost) {
		t.Fatalf("stale worker RefreshLease() error = %v, want lease lost", err)
	}
}

func isolatedTestPool(t *testing.T, ctx context.Context, databaseURL string) *pgxpool.Pool {
	t.Helper()
	admin, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		t.Fatalf("open admin test pool: %v", err)
	}
	schema := fmt.Sprintf("meetnote_test_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+schema); err != nil {
		admin.Close()
		t.Fatalf("create isolated schema: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = admin.Exec(cleanupCtx, "DROP SCHEMA "+schema+" CASCADE")
		admin.Close()
	})

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		t.Fatalf("parse test database config: %v", err)
	}
	poolConfig.ConnConfig.RuntimeParams["search_path"] = schema
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		t.Fatalf("open isolated test pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func testMeetingQuota() domain.MeetingQuota {
	return domain.MeetingQuota{
		MaxMeetings:     100,
		MaxPending:      10,
		MaxStorageBytes: 200 * 1024 * 1024,
	}
}

func newTestMigrationProvider(pool *pgxpool.Pool) (*goose.Provider, func(), error) {
	database := stdlib.OpenDBFromPool(pool)
	provider, err := goose.NewProvider(
		goose.DialectPostgres,
		database,
		os.DirFS("migrations"),
		goose.WithDisableGlobalRegistry(true),
	)
	if err != nil {
		_ = database.Close()
		return nil, nil, err
	}
	return provider, func() {
		_ = provider.Close()
	}, nil
}
