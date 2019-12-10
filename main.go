/*
Copyright 2019 The Perkeep Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

     http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// The gphotos-cdp program uses the Chrome DevTools Protocol to drive a Chrome session
// that downloads your photos stored in Google Photos.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/input"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/chromedp"
	"github.com/chromedp/chromedp/kb"
)

var (
	// TODO(daneroo): New flags after -v: order if merge?
	nItemsFlag        = flag.Int("n", -1, "number of items to download. If negative, get them all.")
	devFlag           = flag.Bool("dev", false, "dev mode. we reuse the same session dir (/tmp/gphotos-cdp), so we don't have to auth at every run.")
	dlDirFlag         = flag.String("dldir", "", "where to write the downloads. defaults to $HOME/Downloads/gphotos-cdp.")
	startFlag         = flag.String("start", "", "skip all photos until this location is reached. for debugging.")
	runFlag           = flag.String("run", "", "the program to run on each downloaded item, right after it is dowloaded. It is also the responsibility of that program to remove the downloaded item, if desired.")
	verboseFlag       = flag.Bool("v", false, "be verbose")
	verboseTimingFlag = flag.Bool("vt", false, "be verbose about timing")
	listFlag          = flag.Bool("list", false, "Only list, do not download any images")
	allFlag           = flag.Bool("all", false, "Ignore -start and <dlDir>/.lastDone, start from oldest photo, implied by -list")
	headlessFlag      = flag.Bool("headless", false, "Start chrome browser in headless mode (cannot do Auth this way).")
)

var tick = 500 * time.Millisecond

func main() {
	flag.Parse()
	if *nItemsFlag == 0 {
		return
	}
	if !*devFlag && *startFlag != "" {
		log.Print("-start only allowed in dev mode")
		return
	}
	if *listFlag {
		*allFlag = true
	}
	if *startFlag != "" && *allFlag {
		log.Print("-start is ignored if -all (implied by -list)")
		return
	}
	s, err := NewSession()
	if err != nil {
		log.Print(err)
		return
	}
	defer s.Shutdown()

	log.Printf("Session Dir: %v", s.profileDir)
	log.Printf("Download Dir: %v", s.dlDir)

	if err := s.cleanDlDir(); err != nil {
		log.Print(err)
		return
	}

	ctx, cancel := s.NewContext()
	defer cancel()

	// enables monitoring of some browser events of interest (verbose only)
	if *verboseFlag {
		registerEventListeners(ctx)
	}

	if err := s.login(ctx); err != nil {
		log.Print(err)
		return
	}

	if err := chromedp.Run(ctx,
		chromedp.ActionFunc(s.firstNav),
		chromedp.ActionFunc(s.navN(*nItemsFlag)),
	); err != nil {
		log.Print(err)
		return
	}
	log.Printf("Number of Observed URLs: %d", len(observedDLURLS))
	fmt.Println("OK")
}

type Session struct {
	parentContext context.Context
	parentCancel  context.CancelFunc
	dlDir         string // dir where the photos get stored
	profileDir    string // user data session dir. automatically created on chrome startup.
	// lastDone is the most recent (wrt to Google Photos timeline) item (its URL
	// really) that was downloaded. If set, it is used as a sentinel, to indicate that
	// we should skip dowloading all items older than this one.
	lastDone string
	// lastPhoto is our termination criteria. It is the first photo in tha album, and hence
	// it is the last photo to be fetched in the current traversal order.
	// this field is not initialized in NewSession, because it can only be determined after authentication (in firstNav)
	lastPhoto string
}

// getLastDone returns the URL of the most recent item that was downloaded in
// the previous run. If any, it should have been stored in dlDir/.lastdone
func getLastDone(dlDir string) (string, error) {
	data, err := ioutil.ReadFile(filepath.Join(dlDir, ".lastdone"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// NewSession should be documented
func NewSession() (*Session, error) {
	var dir string
	if *devFlag {
		dir = filepath.Join(os.TempDir(), "gphotos-cdp")
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, err
		}
	} else {
		var err error
		dir, err = ioutil.TempDir("", "gphotos-cdp")
		if err != nil {
			return nil, err
		}
	}
	dlDir := *dlDirFlag
	if dlDir == "" {
		dlDir = filepath.Join(os.Getenv("HOME"), "Downloads", "gphotos-cdp")
	}
	if err := os.MkdirAll(dlDir, 0700); err != nil {
		return nil, err
	}
	lastDone, err := getLastDone(dlDir)
	if err != nil {
		return nil, err
	}
	// skip navigating to s.lastDone if (-all (implied by -list))
	if lastDone != "" && *allFlag {
		log.Printf("Skipping lastDone (-all): %v", lastDone)
		lastDone = ""
	}

	s := &Session{
		profileDir: dir,
		dlDir:      dlDir,
		lastDone:   lastDone,
	}
	return s, nil
}

// NewContext should be documented
// TODO(daneroo): Just override default options.
func (s *Session) NewContext() (context.Context, context.CancelFunc) {
	allocatorOpts := []chromedp.ExecAllocatorOption{
		chromedp.NoFirstRun,
		chromedp.NoDefaultBrowserCheck,
		chromedp.UserDataDir(s.profileDir),

		chromedp.Flag("disable-background-networking", true),
		chromedp.Flag("enable-features", "NetworkService,NetworkServiceInProcess"),
		chromedp.Flag("disable-background-timer-throttling", true),
		chromedp.Flag("disable-backgrounding-occluded-windows", true),
		chromedp.Flag("disable-breakpad", true),
		chromedp.Flag("disable-client-side-phishing-detection", true),
		chromedp.Flag("disable-default-apps", true),
		chromedp.Flag("disable-dev-shm-usage", true),
		chromedp.Flag("disable-extensions", true),
		chromedp.Flag("disable-features", "site-per-process,TranslateUI,BlinkGenPropertyTrees"),
		chromedp.Flag("disable-hang-monitor", true),
		chromedp.Flag("disable-ipc-flooding-protection", true),
		chromedp.Flag("disable-popup-blocking", true),
		chromedp.Flag("disable-prompt-on-repost", true),
		chromedp.Flag("disable-renderer-backgrounding", true),
		chromedp.Flag("disable-sync", true),
		chromedp.Flag("force-color-profile", "srgb"),
		chromedp.Flag("metrics-recording-only", true),
		chromedp.Flag("safebrowsing-disable-auto-update", true),
		chromedp.Flag("enable-automation", true),
		chromedp.Flag("password-store", "basic"),
		chromedp.Flag("use-mock-keychain", true),
	}
	if *headlessFlag {
		allocatorOpts = append(allocatorOpts, chromedp.Flag("headless", true))
		allocatorOpts = append(allocatorOpts, chromedp.Flag("disable-gpu", true))
	}

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), allocatorOpts...)
	s.parentContext = ctx
	s.parentCancel = cancel
	ctx, cancel = chromedp.NewContext(s.parentContext)
	return ctx, cancel
}

func (s *Session) Shutdown() {
	s.parentCancel()
}

// cleanDlDir removes all files (but not directories) from s.dlDir
func (s *Session) cleanDlDir() error {
	if s.dlDir == "" {
		return nil
	}
	entries, err := ioutil.ReadDir(s.dlDir)
	if err != nil {
		return err
	}
	for _, v := range entries {
		if v.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(s.dlDir, v.Name())); err != nil {
			return err
		}
	}
	return nil
}

// login navigates to https://photos.google.com/ and waits for the user to have
// authenticated (or for 2 minutes to have elapsed).
func (s *Session) login(ctx context.Context) error {
	return chromedp.Run(ctx,
		page.SetDownloadBehavior(page.SetDownloadBehaviorBehaviorAllow).WithDownloadPath(s.dlDir),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if *verboseFlag {
				log.Printf("* login: Pre-navigate")
			}
			return nil
		}),
		chromedp.Navigate("https://photos.google.com/"),
		// when we're not authenticated, the URL is actually
		// https://www.google.com/photos/about/ , so we rely on that to detect when we have
		// authenticated.
		chromedp.ActionFunc(func(ctx context.Context) error {
			tick := time.Second
			timeout := time.Now().Add(2 * time.Minute)
			var location string
			for {
				if time.Now().After(timeout) {
					return errors.New("login: Timeout waiting for authentication")
				}
				if err := chromedp.Location(&location).Do(ctx); err != nil {
					return err
				}
				if location == "https://photos.google.com/" {
					return nil
				}
				if *verboseFlag {
					log.Printf("* login: Not yet authenticated, at: %v", location)
				}
				time.Sleep(tick)
			}
			// return nil // unreachable
		}),
		chromedp.ActionFunc(func(ctx context.Context) error {
			if *verboseFlag {
				log.Printf("* login: Post-navigate")
			}
			return nil
		}),
	)
}

// registerEventListeners enables monitoring several events in the browser in a passive way
// This is currently just for more verbose reporting.
//  - Currently only used to report internal url navigation between photo pages (EventNavigatedWithinDocument)
//  - It was also a way to track initiated downloads: EventDownloadWillBegin, before downloading was removed
//  - Other Events of interest are left in but commented.
// Other events can be found here: https://godoc.org/github.com/chromedp/cdproto/page.
// The events are not reported synchronously.
func registerEventListeners(ctx context.Context) {
	chromedp.ListenTarget(ctx, func(ev interface{}) {
		switch ev := ev.(type) {
		case *page.EventNavigatedWithinDocument:
			log.Printf("* page.EventNavigatedWithinDocument URL:%+v\n", ev.URL)
			// case *target.EventTargetInfoChanged:
			// 	log.Printf("* target.EventTargetInfoChanged URL:%+v\n", ev.TargetInfo.URL)
			// case *page.EventFrameRequestedNavigation:
			// 	log.Printf("* page.EventFrameRequestedNavigation URL:%+v\n", ev.URL)
			// case *cdproto.Message:
			// log.Printf("* cdproto.Message %+v\n", ev)
			// case *page.EventDownloadWillBegin:
			// 	log.Printf("* page.EventDownloadWillBegin URL:%+v\n", ev.URL)
			// case *cruntime.EventConsoleAPICalled:
			// 	fmt.Printf("* console.%s call:\n", ev.Type)
			// 	for _, arg := range ev.Args {
			// 		fmt.Printf("%s - %s\n", arg.Type, arg.Value)
			// 	}
			// case *cruntime.EventExceptionThrown:
			// 	fmt.Printf("* %s\n", ev.ExceptionDetails)
			// 	gotException <- true
			// default:
			//   log.Printf("Got an event %T", ev)
		}
	})

}

// firstNav does either of:
// 1) if a specific photo URL was specified with *startFlag, it navigates to it
// 2) if the last session marked what was the most recent downloaded photo, it navigates to it
// 3) otherwise it jumps to the end of the timeline (i.e. the oldest photo)
func (s *Session) firstNav(ctx context.Context) error {

	// fetch lastPhoto before navigating to specific photoURL
	lastPhoto, err := lastPhoto(ctx)
	if err != nil {
		return err
	}
	s.lastPhoto = lastPhoto

	if *startFlag != "" {
		chromedp.Navigate(*startFlag).Do(ctx)
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		return nil
	}
	if s.lastDone != "" {
		chromedp.Navigate(s.lastDone).Do(ctx)
		chromedp.WaitReady("body", chromedp.ByQuery).Do(ctx)
		return nil
	}

	if err := navToEnd(ctx); err != nil {
		return err
	}

	if err := navToLast(ctx); err != nil {
		return err
	}

	return nil
}

// lastPhoto returns the URL for the first image in the album. It is meant to be our termination criteria, as the photos are traversed in reverse order
func lastPhoto(ctx context.Context) (string, error) {
	// This should be our TerminationCriteria
	// extract most recent photo URL
	// expr := `document.querySelector('a[href^="./photo/"]').href;` // first photo
	// This selector matches <a href="./photo/*"/> elements
	// - where the ^= operator matches prefix of the href attribute
	sel := `a[href^="./photo/"]` // first photo on the landing page
	var href string
	ok := false
	if err := chromedp.AttributeValue(sel, "href", &href, &ok).Do(ctx); err != nil {
		return "", err
	}
	if !ok {
		return "", errors.New("lastPhoto: Unable to find most recent photo")
	}
	if last, err := absURL(href); err != nil {
		return "", err
	} else {
		log.Printf("Last Photo: %s (first on Landing Page)", last)
		return last, nil
	}
}

func absURL(href string) (string, error) {
	// if href is a relative url, e.g.: "./photo/AF1QipMOl0XXrO9WPSv5muLRBFpbyzGsdnrqUqtF8f73"
	// we need an absolute url: https://photos.google.com/photo/AF1QipMOl0XXrO9WPSv5muLRBFpbyzGsdnrqUqtF8f73
	u, err := url.Parse(href)
	if err != nil {
		return "", err
	}
	// the base url could be fetched from the current location, but the login flow already assume that we are at this url
	base, err := url.Parse("https://photos.google.com/")
	if err != nil {
		return "", err
	}
	absURL := base.ResolveReference(u).String()
	return absURL, nil

}

// navToEnd waits for the page to be ready to receive scroll key events, by
// trying to select an item with the right arrow key, and then scrolls down to the
// end of the page, i.e. to the oldest items.
func navToEnd(ctx context.Context) error {
	// wait for page to be loaded, i.e. that we can make an element active by using
	// the right arrow key.
	for {
		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		var ids []cdp.NodeID
		if err := chromedp.Run(ctx,
			chromedp.NodeIDs(`document.activeElement`, &ids, chromedp.ByJSPath)); err != nil {
			return err
		}
		if len(ids) > 0 {
			if *verboseFlag {
				log.Printf("* navToEnd: We are ready, because element %v is selected", ids[0])
			}
			break
		}
		time.Sleep(tick)
	}

	// TODO(daneroo): replace this with a (?faster) DOM Css selector query (a[href^="./photo/"])
	// TODO(daneroo): perhaps follow with RignArrow in Album page ?? or merge with NavToLast
	// TODO(daneroo): 500ms is not always enough between scroll events...
	// try jumping to the end of the page. detect we are there and have stopped
	// moving when two consecutive screenshots are identical.
	var previousScr, scr []byte
	for {
		chromedp.KeyEvent(kb.PageDown).Do(ctx)
		chromedp.KeyEvent(kb.End).Do(ctx)
		chromedp.CaptureScreenshot(&scr).Do(ctx)
		if previousScr == nil {
			previousScr = scr
			continue
		}
		if bytes.Equal(previousScr, scr) {
			break
		}
		previousScr = scr
		time.Sleep(tick)
	}

	if *verboseFlag {
		log.Printf("Successfully jumped to the end")
	}

	return nil
}

// navToLast sends the "\n" event until we detect that an item is loaded as a
// new page. It then sends the right arrow key event until we've reached the very
// last item.
// TODO(daneroo): remove tick sleep, after ArrowRight.. or combine with NavLast?
func navToLast(ctx context.Context) error {
	var location, prevLocation string
	ready := false
	for {
		chromedp.KeyEvent(kb.ArrowRight).Do(ctx)
		time.Sleep(tick)
		if !ready {
			chromedp.KeyEvent("\n").Do(ctx)
			time.Sleep(tick)
		}
		if err := chromedp.Location(&location).Do(ctx); err != nil {
			return err
		}
		if !ready {
			if location != "https://photos.google.com/" {
				ready = true
				log.Printf("Nav to the end sequence is started because location is %v", location)
			}
			continue
		}

		if location == prevLocation {
			break
		}
		log.Printf("NavToLast iteration: location is %v", location)
		prevLocation = location
	}
	return nil
}

// doRun runs *runFlag as a command on the given filePath.
func doRun(filePath string) error {
	if *runFlag == "" {
		return nil
	}
	if *verboseFlag {
		log.Printf("Running %v on %v", *runFlag, filePath)
	}
	cmd := exec.Command(*runFlag, filePath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// navLeft navigates to the next item to the left
func navLeft(ctx context.Context, prevLocation *string) error {
	start := time.Now()
	deadline := start.Add(15 * time.Second) // sleep max of 15 Seconds
	maxBackoff := 500 * time.Millisecond
	backoff := 10 * time.Millisecond // exp backoff: 10,20,40,..,maxBackoff,maxBackoff,..

	// var prevLocation string
	var location string

	chromedp.KeyEvent(kb.ArrowLeft).Do(ctx)

	n := 0
	for {
		if err := chromedp.Location(&location).Do(ctx); err != nil {
			return err
		}
		n++
		if location != *prevLocation {
			break
		}

		// Should return an error, but this is conflated with Termination Condition
		if time.Now().After(deadline) {
			log.Printf("NavLeft timed out after: %s (%d)", time.Since(start), n)
			break
		}

		time.Sleep(backoff)
		if backoff < maxBackoff {
			// calculate next exponential backOff (capped to maxBackoff)
			backoff = backoff * 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
	elapsed := time.Since(start)
	if *verboseTimingFlag && (n > 10 || elapsed.Seconds() > 10) {
		log.Printf(". navLeft n:%d elapsed: %s", n, elapsed)
	}
	return nil
}

// markDone saves location in the dldir/.lastdone file, to indicate it is the
// most recent item downloaded
func markDone(dldir, location string) error {
	if *verboseFlag {
		log.Printf("Marking %v as done", location)
	}
	// TODO(mpl): back up .lastdone before overwriting it, in case writing it fails.
	if err := ioutil.WriteFile(filepath.Join(dldir, ".lastdone"), []byte(location), 0600); err != nil {
		return err
	}
	return nil
}

// startDownload sends the Shift+D event, to start the download of the currently
// viewed item.
func startDownload(ctx context.Context) error {
	keyD, ok := kb.Keys['D']
	if !ok {
		return errors.New("no D key")
	}

	down := input.DispatchKeyEventParams{
		Key:                   keyD.Key,
		Code:                  keyD.Code,
		NativeVirtualKeyCode:  keyD.Native,
		WindowsVirtualKeyCode: keyD.Windows,
		Type:                  input.KeyDown,
		Modifiers:             input.ModifierShift,
	}
	if runtime.GOOS == "darwin" {
		down.NativeVirtualKeyCode = 0
	}
	up := down
	up.Type = input.KeyUp

	for _, ev := range []*input.DispatchKeyEventParams{&down, &up} {
		if *verboseFlag {
			log.Printf("Event: %+v", *ev)
			// log.Printf("Event: %+v", (*ev).Type)
		}
		if err := ev.Do(ctx); err != nil {
			return err
		}
	}
	return nil
}

// dowload starts the download of the currently viewed item, and on successful
// completion saves its location as the most recent item downloaded. It returns
// with an error if the download stops making any progress for more than a minute.
func (s *Session) download(ctx context.Context, location string) (string, error) {

	if err := startDownload(ctx); err != nil {
		return "", err
	}
	elapsedStarted = time.Since(dlAndMoveStart)

	tick := 10 * time.Millisecond

	var filename string
	started := false
	var fileSize int64
	deadline := time.Now().Add(time.Minute)
	for {
		time.Sleep(tick)
		if !started && time.Now().After(deadline) {
			return "", fmt.Errorf("downloading in %q took too long to start", s.dlDir)
		}
		if started && time.Now().After(deadline) {
			return "", fmt.Errorf("hit deadline while downloading in %q", s.dlDir)
		}

		entries, err := ioutil.ReadDir(s.dlDir)
		if err != nil {
			return "", err
		}
		var fileEntries []os.FileInfo
		for _, v := range entries {
			if v.IsDir() {
				continue
			}
			if v.Name() == ".lastdone" {
				continue
			}
			fileEntries = append(fileEntries, v)
		}
		if len(fileEntries) < 1 {
			continue
		}
		if len(fileEntries) > 1 {
			for i := 0; i < len(fileEntries); i++ {
				log.Printf(" - %s", fileEntries[i].Name())
			}
			continue
			// return "", fmt.Errorf("more than one file (%d) in download dir %q", len(fileEntries), s.dlDir)
		}
		if !started {
			if len(fileEntries) > 0 {
				elapsedSawFile = time.Since(dlAndMoveStart)

				started = true
				deadline = time.Now().Add(time.Minute)
			}
		}
		newFileSize := fileEntries[0].Size()
		if newFileSize > fileSize {
			// push back the timeout as long as we make progress
			deadline = time.Now().Add(time.Minute)
			fileSize = newFileSize
		}
		if !strings.HasSuffix(fileEntries[0].Name(), ".crdownload") {
			// download is over
			filename = fileEntries[0].Name()
			break
		}
	}

	if err := markDone(s.dlDir, location); err != nil {
		return "", err
	}

	return filename, nil
}

// moveDownload creates a directory in s.dlDir named of the item ID found in
// location. It then moves dlFile in that directory. It returns the new path
// of the moved file.
func (s *Session) moveDownload(ctx context.Context, dlFile, location string) (string, error) {
	parts := strings.Split(location, "/")
	if len(parts) < 5 {
		return "", fmt.Errorf("not enough slash separated parts in location %v: %d", location, len(parts))
	}
	newDir := filepath.Join(s.dlDir, parts[4])
	if err := os.MkdirAll(newDir, 0700); err != nil {
		return "", err
	}
	newFile := filepath.Join(newDir, dlFile)
	if err := os.Rename(filepath.Join(s.dlDir, dlFile), newFile); err != nil {
		return "", err
	}
	return newFile, nil
}

// TODO(daneroo): Remove along with old download code (or replace with new metrics/channel)
var dlAndMoveStart time.Time
var elapsedStarted time.Duration
var elapsedSawFile time.Duration
var elapsedFileDone time.Duration

// TODO(daneroo): remove dead code,
func (s *Session) dlAndMove(ctx context.Context, location string) (string, error) {

	dlAndMoveStart = time.Now()
	dlFile, err := s.download(ctx, location)
	if err != nil {
		return "", err
	}
	elapsedFileDone = time.Since(dlAndMoveStart)
	ss, ee := s.moveDownload(ctx, dlFile, location)
	elapsedTotal := time.Since(dlAndMoveStart)
	log.Printf("started: %s saw:: %s fileDOne: %s moved: %s file:%s", elapsedStarted, elapsedSawFile, elapsedFileDone, elapsedTotal, dlFile)
	return ss, ee
	// return s.moveDownload(ctx, dlFile, location)
}

// This is a map of previously seen download urls.. might grow very large (number of images)
// It is only used a Set, and to avoid double fetches/downloads, could last n most recent?
// Used in extractDownloadURLFromDOM
var observedDLURLS = map[string]bool{}

// TODO(daneroo) - Rename! Give it a type/Struct
// TODO(daneroo) - explain selector and query boundaries, <c-wiz .../>
// TODO(daneroo) - Also isolate differnet behaviors: optimistic, verify,..
func (s *Session) extractDownloadURLFromDOM(ctx context.Context) error {
	// not sure if this element <c-wiz /> type is stable over time. removing it for now
	// also this seem to be slightly faster without the <cwiz/> element specifier
	// sel := `c-wiz[data-media-key][data-url]`
	sel := `[data-media-key][data-url]`

	var attrs []map[string]string
	if err := chromedp.AttributesAll(sel, &attrs).Do(ctx); err != nil {
		log.Printf("dlAndMove: document.quertSelectorAll:%s error %s", sel, err)
	} else {
		for _, cwiz := range attrs {
			key := cwiz["data-media-key"]
			url := cwiz["data-url"]
			if _, ok := observedDLURLS[url]; !ok {
				observedDLURLS[url] = true
				// fetch the header directly: NOT from chromedp...
				// TODO(daneroo) refactor out, https://golang.org/pkg/net/http/
				// - replace ioutil.ReadAll with Copy to file (with .XYZf7download)
				// res, err := http.Head(url) // NOTE: Actually faster to use GET and close Body, that using HEAD

				start := time.Now()
				res, err := http.Get(url)
				if err != nil {
					log.Printf("dlAndMove: %s : url:...%s... Error: %s", key, url[25:50], err) // could check that src attr exists
					//  continue anyway?
					continue
				}
				defer res.Body.Close()
				contentDisposition := res.Header.Get("content-disposition")
				fname := contentDisposition[17 : len(contentDisposition)-1]
				contentType := res.Header.Get("content-type")
				contentLength := res.ContentLength
				if *verboseFlag {
					log.Printf("* dlAndMove: %s : url:...%s...", key, url[25:50])
					log.Printf("dlAndMove: %s : url:...%s... file:%s mime:%s len:%d", key, url[25:50], fname, contentType, contentLength)
				}

				newDir := filepath.Join(s.dlDir, key)
				if err := os.MkdirAll(newDir, 0700); err != nil {
					return err
				}
				newFile := filepath.Join(newDir, fname)
				// uniq++
				// newFile := fmt.Sprintf("%s.%d", filepath.Join(newDir, fname), uniq)

				out, err := os.Create(newFile)
				defer out.Close()

				n, err := io.Copy(out, res.Body)
				if err != nil {
					return err
				}
				if n != contentLength {
					log.Printf("Warning unexpected file length: expected:%d got:%d", contentLength, n)
				}
				log.Printf("Downloaded %s in %s from %s", newFile, time.Since((start)), url)
			}
		}
	}
	return nil
}

// navN successively downloads the currently viewed item, and navigates to the
// next item (to the left). It repeats N times or until the last (i.e. the most
// recent) item is reached. Set a negative N to repeat until the end is reached.
func (s *Session) navN(N int) func(context.Context) error {
	return func(ctx context.Context) error {
		n := 0
		if N == 0 {
			return nil
		}
		var location, prevLocation string
		totalDuration := 0.0 // for reporting
		batchDuration := 0.0 // for reporting
		batchSize := 1000    // We force a browser reload every batchSize iterations

		for {
			start := time.Now()
			if err := chromedp.Location(&location).Do(ctx); err != nil {
				return err
			}

			// This is still active as a termination criteria, but is pre-empted by location == s.lastPhoto below
			if location == prevLocation {
				break
			}
			prevLocation = location

			if !*listFlag {
				filePath, err := s.dlAndMove(ctx, location)
				if err != nil {
					return err
				}
				if err := doRun(filePath); err != nil {
					return err
				}
			} else {
				err := s.extractDownloadURLFromDOM(ctx)
				if err != nil {
					return err
				}
				if *verboseFlag {
					log.Printf("Listing (%d): %v", n, location)
				}
			}

			n++
			if N > 0 && n >= N {
				break
			}

			// Termination criteria: s.lastPhoto reached
			if location == s.lastPhoto {
				log.Printf("Last photo reached (%d): %v", n, location)
				break
			}
			if err := navLeft(ctx, &prevLocation); err != nil {
				return err
			}
			totalDuration += time.Since(start).Seconds()
			batchDuration += time.Since(start).Seconds()

			// Reload page on batch boundary
			if n%batchSize == 0 {

				// This is where we reload the page - this reduces accumulated latency significantly
				// Page reload taked about 1.3 seconds
				reloadStart := time.Now()
				if err := chromedp.Reload().Do(ctx); err != nil {
					if *verboseTimingFlag {
						log.Printf(". Avg Latency (last %d @ %d):  Marginal: %.2fms Cumulative: %.2fms",
							batchSize, n, batchDuration*1000.0/float64(batchSize), totalDuration*1000.0/float64(n))
					}
					log.Printf("Failed to reload Page at n:%d", n)

					return err
				}

				if *verboseTimingFlag {
					log.Printf(". Avg Latency (last %d @ %d):  Marginal: %.2fms Cumulative: %.2fms Page Reloaded:%s",
						batchSize, n, batchDuration*1000.0/float64(batchSize), totalDuration*1000.0/float64(n), time.Since(reloadStart))
				}
				if *verboseFlag {
					log.Printf("Reloaded page at: %s in %s", location, time.Since(reloadStart))
				}
				batchDuration = 0.0 // reset Statistics
			}
		}
		if *verboseTimingFlag {
			log.Printf("Rate (%d): %.2f/s Avg Latency: %.2fms", n, float64(n)/totalDuration, totalDuration*1000.0/float64(n))
		}
		return nil
	}
}
