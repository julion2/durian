// benchseeder creates distinct encrypted synthetic startup-benchmark mailboxes.
// Content is deterministic, but production AES-GCM uses fresh random nonces.
// The integration testseeder and all production code remain unchanged.
package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/julion2/durian/cli/internal/contacts"
	"github.com/julion2/durian/cli/internal/dbcrypto"
	"github.com/julion2/durian/cli/internal/store"
)

type stats struct {
	Messages, Threads, HTML, Attachments, InboxMessages, InboxThreads int
	TextBytes, HTMLBytes                                              int64
	Accounts                                                          map[string]int
	ThreadLengths                                                     map[int]int
	BodyBuckets                                                       map[string]int
	SelectedBodyMarker                                                string
	SelectedThread                                                    string
	SeedSeconds                                                       float64
	BroadCount                                                        *int
	BroadCountError                                                   string
}

func mixed(n int) uint64 {
	x := uint64(n) + 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

var vocabulary = strings.Fields("planning delivery invoice budget review release routing customer meeting project quality milestone schedule archive documentation design progress analysis discussion integration attachment finance approval research service report requirements summary feedback operations deployment testing calendar contract migration infrastructure performance rollout reliability maintenance estimate proposal onboarding timeline coordination support response followup resolution resource allocation workflow security incident investigation monitoring engineering marketing procurement compliance prototype roadmap metrics inventory newsletter partnership training assessment change request decision notes status action results overview regional weekly quarterly annual information confirmation reference")

func body(index int, size int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "BenchBody %06d. Synthetic correspondence record %d.\n\n", index, index)
	for j := 0; b.Len() < size; j++ {
		fmt.Fprintf(&b, "%s %s %s %s %s %s. ", vocabulary[(index+j*7)%len(vocabulary)], vocabulary[(index*3+j*11)%len(vocabulary)], vocabulary[(index+j*13)%len(vocabulary)], vocabulary[(index*5+j*17)%len(vocabulary)], vocabulary[(index+j*19)%len(vocabulary)], vocabulary[(index*7+j*23)%len(vocabulary)])
		if j%5 == 4 {
			b.WriteString("\n\n")
		}
	}
	return b.String()[:size]
}

func check(err error) {
	if err != nil {
		panic(err)
	}
}

func validate(root string, count int, kr *dbcrypto.Keyring, s *stats) {
	db, err := sql.Open("sqlite", filepath.Join(root, "email.db"))
	check(err)
	check(db.QueryRow("SELECT COUNT(*),COUNT(DISTINCT thread_id),SUM(length(body_text_ct)-29),SUM(COALESCE(length(body_html_ct)-29,0)),COUNT(body_html_ct) FROM messages").Scan(&s.Messages, &s.Threads, &s.TextBytes, &s.HTMLBytes, &s.HTML))
	var indexed, invalid int
	check(db.QueryRow("SELECT COUNT(*) FROM messages_blind_fts").Scan(&indexed))
	check(db.QueryRow("SELECT COUNT(*) FROM messages WHERE substr(body_text_ct,1,1)!=x'01' OR substr(subject_ct,1,1)!=x'01'").Scan(&invalid))
	if s.Messages != count || indexed != count || invalid != 0 {
		panic("independent row/index/encryption-envelope counts mismatch")
	}
	check(db.QueryRow("SELECT COUNT(*) FROM attachments").Scan(&s.Attachments))
	check(db.QueryRow("SELECT COUNT(*),COUNT(DISTINCT thread_id) FROM messages WHERE mailbox_id=1").Scan(&s.InboxMessages, &s.InboxThreads))
	check(db.QueryRow("SELECT thread_id,printf('BenchBody %06d',id-1) FROM messages WHERE mailbox_id=1 ORDER BY date DESC LIMIT 1").Scan(&s.SelectedThread, &s.SelectedBodyMarker))
	s.Accounts, s.ThreadLengths, s.BodyBuckets = map[string]int{}, map[int]int{}, map[string]int{}
	rows, err := db.Query("SELECT a.name,COUNT(*) FROM messages m JOIN accounts a ON a.id=m.account_id GROUP BY a.name")
	check(err)
	for rows.Next() {
		var name string
		var n int
		check(rows.Scan(&name, &n))
		s.Accounts[name] = n
	}
	check(rows.Err())
	check(rows.Close())
	rows, err = db.Query("SELECT n,COUNT(*) FROM (SELECT COUNT(*) n FROM messages GROUP BY thread_id) GROUP BY n")
	check(err)
	for rows.Next() {
		var length, n int
		check(rows.Scan(&length, &n))
		s.ThreadLengths[length] = n
	}
	check(rows.Err())
	check(rows.Close())
	for index := 0; index < count; index++ {
		p, bucket := (mixed(index+1000000)>>16)%100, "512..2048"
		switch {
		case p >= 97:
			bucket = "32768..131072"
		case p >= 85:
			bucket = "8192..32768"
		case p >= 55:
			bucket = "2048..8192"
		}
		s.BodyBuckets[bucket]++
	}
	check(db.Close())
	storeDB, err := store.Open(filepath.Join(root, "email.db"), kr)
	check(err)
	defer storeDB.Close()
	results, err := storeDB.Search("tag:inbox", 200)
	check(err)
	if len(results) != min(200, s.InboxThreads) || results[0].Thread != s.SelectedThread {
		panic("fixture search/thread mismatch")
	}
	n, err := storeDB.SearchCount("BenchBody")
	if err != nil {
		s.BroadCountError = err.Error()
		fmt.Fprintf(os.Stderr, "[BENCH] broad SearchCount(\"BenchBody\") failed: %v\n", err)
	} else {
		broadCount := n
		s.BroadCount = &broadCount
		if n != s.Threads {
			panic("broad body index/thread count mismatch")
		}
	}
	n, err = storeDB.SearchCount(s.SelectedBodyMarker)
	check(err)
	if n != 1 {
		panic("unique body marker did not match exactly one thread")
	}
	messages, err := storeDB.GetByThread(s.SelectedThread)
	check(err)
	if !strings.HasPrefix(messages[len(messages)-1].BodyText, s.SelectedBodyMarker) {
		panic("selected body did not decrypt correctly")
	}
	data, err := json.MarshalIndent(s, "", "  ")
	check(err)
	check(os.WriteFile(filepath.Join(root, "stats.json"), append(data, '\n'), 0o600))
}

func main() {
	args := os.Args[1:]
	if len(args) == 3 && args[0] == "--profile" {
		profileSearch(args[1], args[2])
		return
	}
	verifyOnly := len(args) == 3 && args[0] == "--verify"
	if verifyOnly {
		args = args[1:]
	}
	if len(args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: benchseeder [--verify] <output-directory> <message-count>\n       benchseeder --profile <existing-corpus> <new-profile-directory>")
		os.Exit(2)
	}
	count, err := strconv.Atoi(args[1])
	check(err)
	if count < 100 || count > 1000000 {
		panic("message count must be 100..1000000")
	}
	root := args[0]
	master, err := hex.DecodeString(os.Getenv("DURIAN_MASTER_KEY_HEX_SECRET"))
	check(err)
	if len(master) != dbcrypto.MasterKeyLen {
		panic("set a 32-byte public fixture master key")
	}
	kr, err := dbcrypto.NewKeyring(master)
	check(err)
	if verifyOnly {
		s := stats{}
		validate(root, count, kr, &s)
		fmt.Printf("Verified %d encrypted messages, %d threads, %d attachments\n", s.Messages, s.Threads, s.Attachments)
		return
	}
	check(os.Mkdir(root, 0o700)) // Refuse to overwrite any existing data.
	path := filepath.Join(root, "email.db")
	storeDB, err := store.Open(path, kr)
	check(err)
	check(storeDB.Init())
	check(storeDB.Close())
	contactDB, err := contacts.Open(filepath.Join(root, "contacts.db"))
	check(err)
	check(contactDB.Init())
	for i := 0; i < 240; i++ {
		check(contactDB.Add(fmt.Sprintf("person%03d@synthetic.invalid", i), fmt.Sprintf("Synthetic Person %03d", i), "imported"))
	}
	check(contactDB.Close())

	// Benchmark-only bulk loading: store.Init owns the schema, Keyring owns all
	// encryption and TokenizeFTS owns blind search. Avoid the public insert API's
	// quadratic reverse-reference lookup. Every row is newly generated/encrypted;
	// there are no copied ciphertexts, real addresses, provider refs or pending work.
	db, err := sql.Open("sqlite", path)
	check(err)
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=NORMAL; PRAGMA foreign_keys=ON")
	check(err)
	accounts := []string{"alpha", "beta", "gamma"}
	for i, name := range accounts {
		ct, err := kr.EncryptMeta([]byte(name))
		check(err)
		_, err = db.Exec("INSERT INTO accounts(id,name,name_ct) VALUES(?,?,?)", i+1, name, ct)
		check(err)
	}
	for i, name := range []string{"INBOX", "Sent", "Archive"} {
		ct, err := kr.EncryptMeta([]byte(name))
		check(err)
		_, err = db.Exec("INSERT INTO mailboxes(id,name,name_ct) VALUES(?,?,?)", i+1, name, ct)
		check(err)
	}
	s := stats{Accounts: map[string]int{}, ThreadLengths: map[int]int{}, BodyBuckets: map[string]int{}}
	started := time.Now()
	var tx *sql.Tx
	var insert, tag, fts, attachment *sql.Stmt
	begin := func() {
		tx, err = db.Begin()
		check(err)
		insert, err = tx.Prepare(`INSERT INTO messages(id,stable_id,message_id,thread_id,in_reply_to,refs,subject_ct,from_addr,to_addrs,cc_addrs,date,created_at,body_text_ct,body_html_ct,mailbox_id,account_id,is_seen,is_flagged,is_deleted,uid,size,fetched_body,remote_ref,synthetic_identity,ingest_pending,ingest_generation,synced_flags) VALUES(?,?,?,?,?,?,?,?,?,?,?, ?,?,?,?,?,?,?,0,?,?,1,'',0,0,0,'')`)
		check(err)
		tag, err = tx.Prepare("INSERT INTO tags(message_id,tag) VALUES(?,?)")
		check(err)
		fts, err = tx.Prepare("INSERT INTO messages_blind_fts(rowid,subject_tok,from_tok,to_tok,body_tok) VALUES(?,?,?,?,?)")
		check(err)
		attachment, err = tx.Prepare("INSERT INTO attachments(message_db_id,part_id,filename_ct,content_type_ct,size_ct,disposition,content_id) VALUES(?,?,?,?,?,?,?)")
		check(err)
	}
	commit := func() {
		check(insert.Close())
		check(tag.Close())
		check(fts.Close())
		check(attachment.Close())
		check(tx.Commit())
	}
	begin()
	index, thread := 0, 0
	for index < count {
		h := mixed(thread)
		length := 1
		switch p := h % 100; {
		case p >= 95:
			length = 16
		case p >= 82:
			length = 8
		case p >= 61:
			length = 4
		case p >= 32:
			length = 2
		}
		if length > count-index {
			length = count - index
		}
		account := 0
		switch p := (h >> 8) % 100; {
		case p >= 89:
			account = 2
		case p >= 62:
			account = 1
		}
		mailbox := 1
		switch p := (h >> 16) % 100; {
		case p >= 79:
			mailbox = 3
		case p >= 58:
			mailbox = 2
		}
		if mailbox == 1 {
			s.InboxThreads++
		}
		rootID := fmt.Sprintf("bench-%06d-00@synthetic.invalid", thread)
		digest := sha256.Sum256([]byte(rootID))
		threadID := fmt.Sprintf("%x", digest[:8])
		s.ThreadLengths[length]++
		s.Threads++
		for part := 0; part < length; part++ {
			m := mixed(index + 1000000)
			size, bucket := 512+int(m%1537), "512..2048"
			switch p := (m >> 16) % 100; {
			case p >= 97:
				size, bucket = 32768+int(m%98305), "32768..131072"
			case p >= 85:
				size, bucket = 8192+int(m%24577), "8192..32768"
			case p >= 55:
				size, bucket = 2048+int(m%6145), "2048..8192"
			}
			text := body(index, size)
			s.BodyBuckets[bucket]++
			s.TextBytes += int64(len(text))
			html := ""
			if (m>>32)%100 < 41 {
				html = "<html><body><p>" + strings.ReplaceAll(text, "\n\n", "</p><p>") + "</p><table><tr><td>synthetic status</td><td>" + strconv.Itoa(index) + "</td></tr></table></body></html>"
				s.HTML++
				s.HTMLBytes += int64(len(html))
			}
			subject := fmt.Sprintf("BenchThread %06d %s %s", thread, vocabulary[thread%len(vocabulary)], vocabulary[(thread*7+3)%len(vocabulary)])
			if part != 0 {
				subject = "Re: " + subject
			}
			from := fmt.Sprintf("Person %03d <person%03d@synthetic.invalid>", (thread+part*7)%240, (thread+part*7)%240)
			to := accounts[account] + "@synthetic.invalid"
			id := fmt.Sprintf("bench-%06d-%02d@synthetic.invalid", thread, part)
			reply, refs := "", ""
			if part > 0 {
				reply = fmt.Sprintf("<bench-%06d-%02d@synthetic.invalid>", thread, part-1)
				refs = "<" + rootID + ">"
			}
			seen, flagged := 1, 0
			if m%7 == 0 {
				seen = 0
			}
			if h%19 == 0 {
				flagged = 1
			}
			subjectCT, err := kr.EncryptSubject([]byte(subject))
			check(err)
			textCT, err := kr.EncryptBody([]byte(text))
			check(err)
			var htmlCT []byte
			if html != "" {
				htmlCT, err = kr.EncryptBody([]byte(html))
				check(err)
			}
			date := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC).Unix() - int64(count-index)*897
			_, err = insert.Exec(index+1, "synthetic-object-"+strconv.Itoa(index), id, threadID, reply, refs, subjectCT, from, to, "", date, date, textCT, htmlCT, mailbox, account+1, seen, flagged, index+1, len(text)+len(html))
			check(err)
			for _, t := range []string{[]string{"inbox", "sent", "archive"}[mailbox-1], fmt.Sprintf("project-%02d", thread%13)} {
				_, err = tag.Exec(index+1, t)
				check(err)
			}
			if seen == 0 {
				_, err = tag.Exec(index+1, "unread")
				check(err)
			}
			if flagged == 1 {
				_, err = tag.Exec(index+1, "flagged")
				check(err)
			}
			if h%11 == 0 {
				_, err = tag.Exec(index+1, "finance")
				check(err)
			}
			_, err = fts.Exec(index+1, dbcrypto.TokenizeFTS(kr.FTSToken, subject), dbcrypto.TokenizeFTS(kr.FTSToken, from), dbcrypto.TokenizeFTS(kr.FTSToken, to), dbcrypto.TokenizeFTS(kr.FTSToken, text))
			check(err)
			if m%100 < 23 {
				files := 1 + int((m>>40)%3)
				for file := 1; file <= files; file++ {
					filename, err := kr.EncryptMeta([]byte(fmt.Sprintf("report-%06d-%d.pdf", index, file)))
					check(err)
					kind, err := kr.EncryptMeta([]byte("application/pdf"))
					check(err)
					bytesCT, err := kr.EncryptMeta([]byte(strconv.Itoa(1024 + int((m>>8)%8000000))))
					check(err)
					_, err = attachment.Exec(index+1, file, filename, kind, bytesCT, "attachment", "")
					check(err)
					s.Attachments++
				}
			}
			s.Accounts[accounts[account]]++
			s.Messages++
			if mailbox == 1 {
				s.InboxMessages++
				s.SelectedBodyMarker = fmt.Sprintf("BenchBody %06d", index)
				s.SelectedThread = threadID
			}
			index++
			if index%1000 == 0 {
				commit()
				fmt.Fprintf(os.Stderr, "[BENCH] seeded %d/%d messages\n", index, count)
				if index < count {
					begin()
				}
			}
		}
		thread++
	}
	if count%1000 != 0 {
		commit()
	}
	_, err = db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	check(err)
	check(db.Close())
	s.SeedSeconds = time.Since(started).Seconds()
	validate(root, count, kr, &s)
	fmt.Printf("Seeded %d encrypted messages, %d distinct threads, %d attachments in %.2fs\n", s.Messages, s.Threads, s.Attachments, s.SeedSeconds)
}

// profileSearch measures the real store without HTTP or a GUI. It never mutates
// the corpus, and refuses to overwrite an earlier evidence directory.
func profileSearch(root, output string) {
	master, err := hex.DecodeString(os.Getenv("DURIAN_MASTER_KEY_HEX_SECRET"))
	check(err)
	kr, err := dbcrypto.NewKeyring(master)
	check(err)
	db, err := store.OpenReadOnly(filepath.Join(root, "email.db"), kr)
	check(err)
	defer db.Close()
	check(os.Mkdir(output, 0o700))
	profile, err := os.Create(filepath.Join(output, "cpu.pprof"))
	check(err)
	defer profile.Close()
	check(pprof.StartCPUProfile(profile))
	defer pprof.StopCPUProfile()
	type observation struct {
		Query      string
		Count      bool
		Run, Rows  int
		Millis     float64
		AllocBytes uint64
		ThreadIDs  []string
	}
	var results []observation
	for _, query := range []string{"tag:inbox", "BenchBody"} {
		for _, count := range []bool{false, true} {
			for run := 1; run <= 3; run++ {
				runtime.GC()
				var before, after runtime.MemStats
				runtime.ReadMemStats(&before)
				result := observation{Query: query, Count: count, Run: run}
				started := time.Now()
				if count {
					result.Rows, err = db.SearchCount(query)
				} else {
					var rows []store.SearchResult
					rows, err = db.Search(query, 200)
					result.Rows = len(rows)
					for _, row := range rows {
						result.ThreadIDs = append(result.ThreadIDs, row.Thread)
					}
				}
				check(err)
				result.Millis = float64(time.Since(started).Microseconds()) / 1000
				runtime.ReadMemStats(&after)
				result.AllocBytes = after.TotalAlloc - before.TotalAlloc
				results = append(results, result)
				fmt.Printf("[BENCH] query=%q count=%v run=%d rows=%d ms=%.3f allocated=%d\n", query, count, run, result.Rows, result.Millis, result.AllocBytes)
			}
		}
	}
	data, err := json.MarshalIndent(results, "", "  ")
	check(err)
	check(os.WriteFile(filepath.Join(output, "results.json"), append(data, '\n'), 0o600))
}
