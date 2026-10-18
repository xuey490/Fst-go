package proxy

import (
	"net/netip"
	"testing"

	"golang.org/x/net/dns/dnsmessage"
)

// TestParseAAnswer 验证出口应答解析：A 记录被取出，错误返回码视为失败。
func TestParseAAnswer(t *testing.T) {
	name, err := dnsmessage.NewName("example.org.")
	if err != nil {
		t.Fatal(err)
	}
	msg := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeSuccess},
		Questions: []dnsmessage.Question{{Name: name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}},
	}
	b := dnsmessage.NewBuilder(nil, msg.Header)
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		t.Fatal(err)
	}
	if err := b.Question(msg.Questions[0]); err != nil {
		t.Fatal(err)
	}
	if err := b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	if err := b.AResource(dnsmessage.ResourceHeader{Name: name, Class: dnsmessage.ClassINET, TTL: 30},
		dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}}); err != nil {
		t.Fatal(err)
	}
	raw, err := b.Finish()
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseAnswer(raw, "ip4")
	if err != nil || len(got) != 1 || got[0] != netip.MustParseAddr("203.0.113.7") {
		t.Fatalf("A answer = %v %v", got, err)
	}

	fail := dnsmessage.Message{
		Header:    dnsmessage.Header{Response: true, RCode: dnsmessage.RCodeServerFailure},
		Questions: msg.Questions,
	}
	bad, err := fail.Pack()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parseAnswer(bad, "ip4"); err == nil {
		t.Fatal("SERVFAIL response must be an error")
	}
}

// TestPackAQuery 验证查询报文可被 DNS 解析器读回，且域名非法时报错。
func TestPackAQuery(t *testing.T) {
	raw, err := packQuery("example.org", "ip4")
	if err != nil {
		t.Fatal(err)
	}
	var p dnsmessage.Parser
	hdr, err := p.Start(raw)
	if err != nil {
		t.Fatal(err)
	}
	if hdr.Response || !hdr.RecursionDesired {
		t.Fatalf("header = %+v", hdr)
	}
	qs, err := p.AllQuestions()
	if err != nil || len(qs) != 1 || qs[0].Type != dnsmessage.TypeA || qs[0].Name.String() != "example.org." {
		t.Fatalf("questions = %+v %v", qs, err)
	}
	if _, err := packQuery("bad name with spaces", "ip4"); err == nil {
		t.Fatal("invalid name must be rejected")
	}
}
