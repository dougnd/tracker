package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/everdev/mack"
	"github.com/getlantern/systray"
	"github.com/pkg/errors"
)

var username = flag.String("username", "", "Your JIRA username")

const jiraTimeFormat = "2006-01-02T15:04:05.00-0700"

type Issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary string `json:"summary"`
	} `json:"fields"`
}

type JiraMenu struct {
	issueItems     map[string]*systray.MenuItem
	issueClickedCh chan string
	selectedKey    string
	sync.RWMutex
}

func NewJiraMenu() *JiraMenu {
	menu := &JiraMenu{
		issueItems:     make(map[string]*systray.MenuItem),
		issueClickedCh: make(chan string),
	}
	menu.refresh()
	return menu
}

func (menu *JiraMenu) isItemPresent(key string) bool {
	menu.RLock()
	_, ok := menu.issueItems[key]
	menu.RUnlock()
	return ok
}

func (menu *JiraMenu) addItem(issue *Issue) {
	if menu.isItemPresent(issue.Key) {
		return
	}
	menu.Lock()
	item := systray.AddMenuItem(issue.Key+" - "+issue.Fields.Summary, issue.Fields.Summary)
	go func() {
		for range item.ClickedCh {
			menu.issueClickedCh <- issue.Key
		}
	}()
	menu.issueItems[issue.Key] = item
	menu.Unlock()
}

func (menu *JiraMenu) refresh() {
	issues, err := getIssues()
	if err != nil {
		log.Print(err)
	}

	for i := range issues {
		menu.addItem(&issues[i])
	}
}

func (menu *JiraMenu) check(key string) {
	menu.selectedKey = ""
	menu.RLock()
	for _, item := range menu.issueItems {
		item.Uncheck()
	}
	if item, ok := menu.issueItems[key]; ok {
		item.Check()
		menu.selectedKey = key
	}
	menu.RUnlock()
}

func getIssues() ([]Issue, error) {
	_, err := exec.Command("jira", "login", "-u", *username).Output()
	if err != nil {
		return nil, errors.Wrap(err, "could not login")
	}
	out, err := exec.Command("jira", "list", "--template", "debug", "--query", "resolution = unresolved and assignee=currentuser() ORDER BY priority asc, created").Output()
	if err != nil {
		return nil, errors.Wrap(err, "could not list assigned issues")
	}

	type ListOut struct {
		Issues []Issue `json:"issues"`
	}

	var listOut ListOut
	err = json.Unmarshal(out, &listOut)

	return listOut.Issues, err
}

func main() {
	flag.Parse()
	if *username == "" {
		user := os.Getenv("USER")
		username = &user
	}
	log.Printf("Using JIRA username %v.", *username)

	onExit := func() {
		fmt.Println("Starting onExit")
	}
	_ = onExit

	// Should be called at the very beginning of main().
	systray.Run(onReady, onExit)
}

type Timer struct {
	ticker    *time.Ticker
	startTime time.Time
	done      chan struct{}
}

func (t *Timer) start() {
	t.startTime = time.Now()
	t.ticker = time.NewTicker(time.Second)
	t.done = make(chan struct{})
	go func() {
		for {
			select {
			case <-t.ticker.C:
				d := time.Since(t.startTime).Round(time.Second).String()
				systray.SetTitle(d)
			case <-t.done:
				return
			}
		}
	}()
}

func (t *Timer) stop() time.Duration {
	d := time.Since(t.startTime)
	t.ticker.Stop()
	close(t.done)
	systray.SetTitle("")
	return d
}

func addWorkLog(key string, started time.Time, d time.Duration) error {
	resp, err := mack.Dialog("Worklog message for "+key+" #"+d.Round(time.Second).String(), "Worklog", " ")
	if err != nil {
		return errors.Wrap(err, "could not ask for log messge")
	}
	if resp.Clicked != "OK" {
		return nil
	}
	log.Println("Adding worklog for ", key, ", message: ", resp.Text)

	_, err = exec.Command("jira", "login", "-u", *username).Output()
	if err != nil {
		return errors.Wrap(err, "could not login!")
	}

	startTime := started.Format(jiraTimeFormat)
	durationTime := strconv.Itoa(int(d.Round(time.Minute).Minutes())) + "m"
	if durationTime == "0m" {
		return errors.New("Cannot record a time log of < 1 minute")
	}

	var out []byte
	if strings.TrimSpace(resp.Text) != "" {
		out, err = exec.Command("jira", "worklog", "add", "--noedit", "--template", "debug",
			"-m", resp.Text, "-S", startTime, "-T", durationTime, key).CombinedOutput()
	} else {
		out, err = exec.Command("jira", "worklog", "add", "--noedit", "--template", "debug",
			"-S", startTime, "-T", durationTime, key).CombinedOutput()
	}
	if err != nil {
		return errors.Wrapf(err, "could not add worklog: %s", string(out))
	}
	return nil
}

func handleTimer(timerItem *systray.MenuItem, menu *JiraMenu) {
	timerStarted := false
	var timer Timer
	for range timerItem.ClickedCh {
		if timerStarted {
			d := timer.stop()
			fmt.Println(d)
			err := addWorkLog(menu.selectedKey, timer.startTime, d)
			if err != nil {
				mack.Alert(err.Error())
			}
			timerStarted = false
			timerItem.SetTitle("Start Timer")
		} else {
			if menu.selectedKey == "" {
				mack.Alert("No Issue Selected")
				continue
			}
			timerItem.SetTitle("Stop Timer")
			timerStarted = true
			timer.start()
		}
	}
}

func addIssue(menu *JiraMenu) {
	resp, err := mack.Dialog("Issue Key?", "Issue Key", "MY-462")
	if err != nil {
		return
	}
	if resp.Clicked != "OK" {
		return
	}

	if menu.isItemPresent(resp.Text) {
		mack.Alert("Issue already there!")
		return
	}

	_, err = exec.Command("jira", "login", "-u", *username).Output()
	if err != nil {
		return
	}
	out, err := exec.Command("jira", "view", resp.Text, "--template", "debug").Output()
	if err != nil {
		mack.Alert("Could not find that issue!")
		return
	}
	var issue Issue
	err = json.Unmarshal(out, &issue)
	if err != nil {
		mack.Alert("Failed to read issue info.  Are you sure it exists?")
		return
	}

	menu.addItem(&issue)
}

func onReady() {
	systray.SetIcon(icon)
	systray.SetTitle("")
	systray.SetTooltip("Jira Time Tracker")
	mQuitOrig := systray.AddMenuItem("Quit", "Quit the whole app")
	go func() {
		<-mQuitOrig.ClickedCh
		fmt.Println("Requesting quit")
		systray.Quit()
		fmt.Println("Finished quitting")
	}()

	timerItem := systray.AddMenuItem("Start Timer", "Start Timer")
	addItem := systray.AddMenuItem("Add Issue", "Add Issue")
	refreshItem := systray.AddMenuItem("Refresh Assigned", "Refresh Assigned")

	systray.AddSeparator()

	menu := NewJiraMenu()
	go handleTimer(timerItem, menu)
	go func() {
		for key := range menu.issueClickedCh {
			fmt.Println(key)
			menu.check(key)
		}
	}()
	go func() {
		for range addItem.ClickedCh {
			addIssue(menu)
		}
	}()
	go func() {
		for range refreshItem.ClickedCh {
			menu.refresh()
		}
	}()
}
