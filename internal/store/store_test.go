package store

import "testing"

// TestMessageRecallAndAttach 验证 kind/attach 落库读取、撤回清空正文，以及跨会话撤回被拒绝。
func TestMessageRecallAndAttach(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	m := Message{
		ID: "m1", PeerID: "p1", Direction: "in", Content: "hi", Status: "delivered",
		TS: Now(), Kind: "image", Attach: []string{"t1", "t2"},
	}
	if _, err := s.InsertMessage(m); err != nil {
		t.Fatal(err)
	}
	got, ok := s.Message("m1")
	if !ok || got.Kind != "image" || len(got.Attach) != 2 || got.Attach[1] != "t2" || got.Recalled {
		t.Fatalf("stored message = %+v", got)
	}
	if changed, err := s.RecallMessage("m1", "someone-else"); err != nil || changed {
		t.Fatalf("foreign recall = %v %v", changed, err)
	}
	if changed, err := s.RecallMessage("m1", "p1"); err != nil || !changed {
		t.Fatalf("recall = %v %v", changed, err)
	}
	got, _ = s.Message("m1")
	if !got.Recalled || got.Content != "" {
		t.Fatalf("after recall = %+v", got)
	}
	if changed, _ := s.RecallMessage("m1", "p1"); changed {
		t.Fatal("second recall must be a no-op")
	}
	list, err := s.Messages("p1", 0, 10)
	if err != nil || len(list) != 1 || !list[0].Recalled {
		t.Fatalf("list = %+v %v", list, err)
	}
}

// TestDoneTransferBySHA 验证「同摘要已完成入站文件」检索：摘要、大小、对端都必须一致。
func TestDoneTransferBySHA(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	err = s.InsertTransfer(Transfer{
		ID: "t1", BatchID: "b1", PeerID: "p1", Direction: "in", FileName: "a.txt",
		Size: 3, SHA256: "abc", LocalPath: "/tmp/a.txt", Status: "done",
		CreatedAt: Now(), FinishedAt: Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := s.DoneTransferBySHA("p1", "abc", 3); !ok || got.ID != "t1" {
		t.Fatalf("hit = %+v %v", got, ok)
	}
	if _, ok := s.DoneTransferBySHA("p1", "abc", 4); ok {
		t.Fatal("size mismatch must miss")
	}
	if _, ok := s.DoneTransferBySHA("p2", "abc", 3); ok {
		t.Fatal("peer mismatch must miss")
	}
	if _, ok := s.DoneTransferBySHA("p1", "", 3); ok {
		t.Fatal("empty digest must miss")
	}
}

// TestOfflineQueue 验证离线队列只取到期、未撤回的消息，并按时间顺序返回；重试记录可被重置。
func TestOfflineQueue(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	now := Now()
	for _, m := range []Message{
		{ID: "q1", PeerID: "p1", Direction: "out", Content: "one", Status: "queued", TS: now},
		{ID: "q2", PeerID: "p1", Direction: "out", Content: "two", Status: "queued", TS: now + 1},
		{ID: "q3", PeerID: "p2", Direction: "out", Content: "three", Status: "queued", TS: now + 2},
	} {
		if _, err := s.InsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetMessageRetry("q2", 2, now+10_000); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMessageStatus("q3", "delivered"); err != nil {
		t.Fatal(err)
	}
	list, err := s.DueMessages(now+5_000, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].ID != "q1" || list[0].Status != "queued" {
		t.Fatalf("due = %+v", list)
	}
	if err := s.QueueMessage("q2"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Message("q2")
	if got.Attempts != 0 || got.NextRetry != 0 {
		t.Fatalf("reset = %+v", got)
	}
}

// TestRoomMessages 验证群聊消息去重、房间预览/未读数与已读标记。
func TestRoomMessages(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if err := s.UpsertRoom(Room{ID: "lan", Name: "局域网", Kind: "lan"}); err != nil {
		t.Fatal(err)
	}
	inserted, err := s.InsertRoomMessage(RoomMessage{
		ID: "r1", RoomID: "lan", SenderID: "peer-a", SenderName: "A", Content: "hello", TS: 10,
	})
	if err != nil || !inserted {
		t.Fatalf("insert = %v %v", inserted, err)
	}
	if inserted, err := s.InsertRoomMessage(RoomMessage{
		ID: "r1", RoomID: "lan", SenderID: "peer-a", SenderName: "A", Content: "duplicate", TS: 11,
	}); err != nil || inserted {
		t.Fatalf("dedupe = %v %v", inserted, err)
	}
	if _, err := s.InsertRoomMessage(RoomMessage{
		ID: "r2", RoomID: "lan", SenderID: "self", SenderName: "Me", Content: "hi", TS: 12,
	}); err != nil {
		t.Fatal(err)
	}
	rooms, err := s.Rooms("self")
	if err != nil || len(rooms) != 1 {
		t.Fatalf("rooms = %+v %v", rooms, err)
	}
	if rooms[0].LastContent != "hi" || rooms[0].Unread != 1 {
		t.Fatalf("room = %+v", rooms[0])
	}
	list, err := s.RoomMessages("lan", 0, 10)
	if err != nil || len(list) != 2 || list[0].ID != "r1" || list[1].ID != "r2" {
		t.Fatalf("messages = %+v %v", list, err)
	}
	if err := s.MarkRoomRead("lan"); err != nil {
		t.Fatal(err)
	}
	rooms, _ = s.Rooms("self")
	if rooms[0].Unread != 0 {
		t.Fatalf("unread after mark = %d", rooms[0].Unread)
	}
}

// TestReplyColumns 验证引用字段能落库并按保存顺序回读（普通消息保持空串）。
func TestReplyColumns(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, m := range []Message{
		{ID: "m1", PeerID: "peer-a", Direction: "out", Content: "question", Status: "delivered", TS: 1, Read: true},
		{ID: "m2", PeerID: "peer-a", Direction: "in", Content: "answer", Status: "delivered", TS: 2,
			ReplyID: "m1", ReplyPreview: "question", ReplySender: "Alice"},
	} {
		if _, err := s.InsertMessage(m); err != nil {
			t.Fatal(err)
		}
	}
	list, err := s.Messages("peer-a", 0, 10)
	if err != nil || len(list) != 2 {
		t.Fatalf("messages = %+v err=%v", list, err)
	}
	if list[0].ReplyID != "" || list[0].ReplyPreview != "" {
		t.Fatalf("plain message has reply: %+v", list[0])
	}
	got, ok := s.Message("m2")
	if !ok || got.ReplyID != "m1" || got.ReplyPreview != "question" || got.ReplySender != "Alice" {
		t.Fatalf("reply lost: %+v ok=%v", got, ok)
	}
}

// TestDeleteTransfers 验证传输日志清理：跳过运行中记录与被聊天附件引用的记录。
func TestDeleteTransfers(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, tr := range []Transfer{
		{ID: "t-done", Status: "done"},
		{ID: "t-failed", Status: "failed"},
		{ID: "t-running", Status: "running"},
		{ID: "t-attached", Status: "done"},
	} {
		tr.PeerID, tr.Direction, tr.FileName, tr.CreatedAt = "p1", "out", tr.ID, Now()
		if err := s.InsertTransfer(tr); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.InsertMessage(Message{
		ID: "m1", PeerID: "p1", Direction: "out", Content: "", Status: "sent", TS: Now(),
		Kind: "image", Attach: []string{"t-attached"},
	}); err != nil {
		t.Fatal(err)
	}
	n, err := s.DeleteTransfers([]string{"t-done", "t-running", "t-attached"})
	if err != nil || n != 1 {
		t.Fatalf("selected delete = %d %v, want 1", n, err)
	}
	if _, ok := s.Transfer("t-done"); ok {
		t.Fatal("finished unowned record must be deleted")
	}
	if _, ok := s.Transfer("t-attached"); !ok {
		t.Fatal("chat attachment must be kept")
	}
	if n, err := s.DeleteTransfers(nil); err != nil || n != 1 {
		t.Fatalf("clear all = %d %v, want 1 (only t-failed)", n, err)
	}
	if _, ok := s.Transfer("t-running"); !ok {
		t.Fatal("running record must be kept")
	}
}
