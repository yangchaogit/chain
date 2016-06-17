package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"

	"chain/env"
)

var (
	listen    = env.String("LISTEN", ":4567")
	db1URL    = env.String("DB1_URL", "postgres:///core?sslmode=disable")
	db2URL    = env.String("DB2_URL", "postgres:///core-2?sslmode=disable")
	db3URL    = env.String("DB3_URL", "postgres:///core-3?sslmode=disable")
	slackURL  = os.Getenv("SLACK_WEBHOOK_URL")
	sourcedir = os.Getenv("CHAIN")
	mu        sync.Mutex
)

type Req struct {
	Ref     string
	After   string
	Log     string
	Commits []struct {
		Message string
		URL     string
		Author  struct {
			Username string
		}
	}
}

func main() {
	log.SetFlags(log.Lshortfile)
	http.HandleFunc("/push", handler)
	log.Println("listening on", *listen)
	log.Fatal(http.ListenAndServe(*listen, nil))
}

func handler(w http.ResponseWriter, r *http.Request) {
	decoder := json.NewDecoder(r.Body)
	req := Req{}
	err := decoder.Decode(&req)
	if err != nil {
		body, err := json.Marshal(map[string]string{"text": fmt.Sprintln("parsing request:", err)})
		if err != nil {
			panic(err)
		}
		postToSlack(body)
		return
	}
	if len(req.Commits) != 1 {
		body, err := json.Marshal(map[string]string{"text": "expecting 1 commit"})
		if err != nil {
			panic(err)
		}
		postToSlack(body)
		return
	}

	log.Println("ref pushed:", req.Ref)
	if req.Ref == "refs/heads/main" {
		go func() {
			mu.Lock()
			defer mu.Unlock()
			defer catch()
			runIn(sourcedir, exec.Command("git", "fetch", "origin"), req)
			runIn(sourcedir, exec.Command("git", "clean", "-xdf"), req)
			runIn(sourcedir, exec.Command("git", "checkout", req.After), req)
			runIn(sourcedir, exec.Command("git", "reset", "--hard", req.After), req)
			runIn(sourcedir, exec.Command("go", "install", "./cmd/cored"), req)
			runIn(sourcedir, exec.Command("go", "install", "./cmd/migratedb"), req)
			runIn(sourcedir, exec.Command("migratedb", "-d", *db1URL), req)
			runIn(sourcedir, exec.Command("migratedb", "-d", *db2URL), req)
			runIn(sourcedir, exec.Command("migratedb", "-d", *db3URL), req)
			runIn(sourcedir+"/qa/tests", exec.Command("mvn", "package"), req)
			runIn(sourcedir, exec.Command("./qa/bin/test-singlecore"), req)
			runIn(sourcedir, exec.Command("./qa/bin/test-multicore"), req)
			postToSlack(buildBody(req))
		}()
	}
}

func buildBody(req Req) []byte {
	var color, result string
	if req.Log == "" {
		color = "good"
		result = "passed :thumbsup:"
	} else {
		color = "danger"
		result = "failed :thumbsdown:"
	}

	buffer := `{
		"attachments": [
			{
				"color": "` + color + `",
				"text": "Integration tests ` + result + `",
				"fields": [
					{
						"title": "Commit",
						"value": "<` + req.Commits[0].URL + `|` + req.Commits[0].Message + `>",
						"short": false
					},
					{
						"title": "Author",
						"value": "<https://github.com/` + req.Commits[0].Author.Username + `|` + req.Commits[0].Author.Username + `>",
						"short": false
					}
				]
			}`

	if req.Log == "" {
		// end json buffer
		buffer += `]}`
	} else {
		// add the error log
		buffer += `,{
			"text": "` + sanitize(req.Log) + `",
			"mrkdwn_in": [
				"text"
			]
		}]}`
	}
	return []byte(buffer)
}

func sanitize(str string) string {
	str = strings.Replace(str, `"`, `\"`, -1)
	str = strings.Replace(str, `&`, `&amp;`, -1)
	str = strings.Replace(str, `<`, `&lt;`, -1)
	str = strings.Replace(str, `>`, `&gt;`, -1)
	return str
}

func runIn(dir string, c *exec.Cmd, req Req) {
	var outbuf, errbuf bytes.Buffer
	c.Dir = dir
	c.Env = os.Environ()
	c.Stdout = &outbuf
	c.Stderr = &errbuf
	if err := c.Run(); err != nil {
		req.Log = fmt.Sprintf("*Command run: *`%s`\n%s", strings.Join(c.Args, " "), errbuf.String())
		panic(buildBody(req))
	}
}

func postToSlack(b []byte) {
	log.Println("sending results to slack")
	resp, err := http.Post(slackURL, "application/json", bytes.NewReader(b))
	if err != nil {
		log.Println("sending request:", err)
	}
	defer resp.Body.Close()
}

func catch() {
	if err := recover(); err != nil {
		switch err := err.(type) {
		case []byte:
			postToSlack(err)
		default:
			panic(err)
		}
	}
}