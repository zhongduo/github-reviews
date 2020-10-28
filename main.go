package main

import (
	"context"
	"flag"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
)

const (
	timeFormat = "1-2-2006"
)

var (
	tokenFile = flag.String("token_file", "", "Path to the token file")
	owner     = flag.String("owner", "knative", "GitHub user name")
	start     = flag.String("start", time.Now().Format(timeFormat), "Start date in '%m-%d-%y' format")
	end       = flag.String("end", time.Now().Format(timeFormat), "End date in %m-%d-%y format")
	numWorker = flag.Int("num_workers", 1, "Number of parallel workers")
	repos     stringSlice
	users     stringSlice

	// The API limit seems to be 5000 requests per hour. So keep after every API request sleep
	// for 0.75 seconds, which should limit the number of requests to 4800 requests per hour.
	sleep           = 750 * time.Millisecond
	parallelWorkers = 1

	retryCount = 5
)

type stringSlice []string

func (ss *stringSlice) String() string {
	return ""
}

func (ss *stringSlice) Set(v string) error {
	*ss = append(*ss, v)
	return nil
}

func main() {
	flag.Var(&repos, "repos", "Repo name")
	flag.Var(&users, "users", "Github users")
	flag.Parse()

	parallelWorkers = *numWorker

	startTime, err := time.Parse(timeFormat, *start)
	if err != nil {
		log.Fatalf("Unable to parse start time '%s': %v", *start, err)
	}
	endTime, err := time.Parse(timeFormat, *end)
	if err != nil {
		log.Fatalf("Unable to parse end time '%s': %v", *end, err)
	}

	log.Printf("Searching for PRs between %v and %v", startTime.Format(timeFormat), endTime.Format(timeFormat))
	client := github.NewClient(oauthClient())
	prs := listPRs(client, startTime)
	log.Printf("Finished listing PRs. %v", len(prs))

	timeFilteredPRs := filterPRsForTime(prs, startTime, endTime)
	log.Printf("Finished filtering PRs for time. %v", len(timeFilteredPRs))

	otherAuthorPRs, authoredPRs := filterPRsForAuthors(timeFilteredPRs, users)
	log.Printf("Finished filtering PRs for authors. %v", len(otherAuthorPRs))

	reviewedPRs := filterPRsForTouch(client, otherAuthorPRs, users)
	log.Printf("Total PRs: %v. Commented PRs: %v", len(otherAuthorPRs), len(reviewedPRs))

	lc := &lineCounter{
		client: client,
		cache:  map[string]int64{},
	}
	totalLinesAdded := lc.added(timeFilteredPRs)
	log.Printf("Total lines added: %v", totalLinesAdded)
	totalNonAuthoredLinesAdded := lc.added(otherAuthorPRs)
	log.Printf("Total non-authored lines added: %v", totalNonAuthoredLinesAdded)
	authoredLinesAdded := lc.added(authoredPRs)
	log.Printf("Total lines authored: %v", authoredLinesAdded)
	reviewedLinesAdded := lc.added(reviewedPRs)
	log.Printf("Total lines reviewed: %v", reviewedLinesAdded)
	if totalNonAuthoredLinesAdded > 0 {
		log.Printf("Percent non-authored lines reviewed: %v", float64(reviewedLinesAdded)/float64(totalNonAuthoredLinesAdded))
	}
	if totalLinesAdded > 0 {
		log.Printf("Percent of all lines authored or reviewed: %v", float64(authoredLinesAdded+reviewedLinesAdded)/float64(totalLinesAdded))
	}
}

func oauthClient() *http.Client {
	oauthToken := readOauthToken()
	ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: oauthToken})
	return oauth2.NewClient(context.Background(), ts)
}

func readOauthToken() string {
	b, err := ioutil.ReadFile(*tokenFile)
	if err != nil {
		log.Fatalf("Unable to read tokenFile, '%s': %v", *tokenFile, err)
	}
	s := string(b)
	return strings.TrimSuffix(s, "\n")
}

func listPRs(client *github.Client, startTime time.Time) []*github.PullRequest {
	prs := make([]*github.PullRequest, 0)
	for _, repo := range repos {
		page := 0
		for {
			p, r, err := retryListUpTo(retryCount, func() ([]*github.PullRequest, *github.Response, error) {
				return client.PullRequests.List(context.TODO(), *owner, repo, &github.PullRequestListOptions{
					State:     "all",
					Sort:      "updated",
					Direction: "desc",
					ListOptions: github.ListOptions{
						Page:    page,
						PerPage: 100,
					},
				})
			})
			if err != nil {
				log.Fatalf("Unable to list PRs for page: %v: %v", page, err)
			}
			prs = append(prs, p...)
			page = r.NextPage
			if page == 0 {
				break
			}
			// Early exit
			if prs[len(prs)-1].UpdatedAt.Before(startTime) {
				break
			}
		}
	}
	return prs
}

func retryListUpTo(count int, f func() ([]*github.PullRequest, *github.Response, error)) ([]*github.PullRequest, *github.Response, error) {
	i := 1
	for {
		p, r, err := f()
		time.Sleep(sleep)
		if err == nil {
			return p, r, nil
		} else if i > count {
			return p, r, err
		}
		i++
	}
}

func filterPRsForTime(unfiltered []*github.PullRequest, startTime time.Time, endTime time.Time) []*github.PullRequest {
	prs := make([]*github.PullRequest, 0)
	for _, pr := range unfiltered {
		if pr.UpdatedAt.After(startTime) && pr.CreatedAt.Before(endTime) {
			prs = append(prs, pr)
		}
	}
	return prs
}

func filterPRsForAuthors(unfiltered []*github.PullRequest, authors []string) ([]*github.PullRequest, []*github.PullRequest) {
	prs := make([]*github.PullRequest, 0)
	authoredPRs := make([]*github.PullRequest, 0)
	for _, pr := range unfiltered {
		if contains(authors, pr.GetUser().GetLogin()) {
			authoredPRs = append(authoredPRs, pr)
		} else {
			prs = append(prs, pr)
		}
	}
	return prs, authoredPRs
}

func contains(set []string, s string) bool {
	for _, str := range set {
		if str == s {
			return true
		}
	}
	return false
}

func filterPRsForTouch(client *github.Client, unfiltered []*github.PullRequest, users []string) []*github.PullRequest {
	input := make(chan *github.PullRequest, len(unfiltered))
	output := make(chan *github.PullRequest, len(unfiltered))
	for i := 0; i < parallelWorkers; i++ {
		go func() {
			for {
				pr := <-input
				if prReviewedBy(client, pr, users) || prCommentedOnBy(client, pr, users) {
					output <- pr
				} else {
					output <- nil
				}
			}
		}()
	}
	for _, pr := range unfiltered {
		input <- pr
	}
	prs := make([]*github.PullRequest, 0)
	for range unfiltered {
		pr := <-output
		if pr != nil {
			prs = append(prs, pr)
		}
	}
	return prs
}

func prCommentedOnBy(client *github.Client, pr *github.PullRequest, users []string) bool {
	page := 0
	for {
		c, r, err := retryListCommentsUpTo(retryCount, func() ([]*github.IssueComment, *github.Response, error) {
			return client.Issues.ListComments(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.IssueListCommentsOptions{
				ListOptions: github.ListOptions{
					Page: page,
				},
			})
		})
		time.Sleep(sleep)
		if err != nil {
			log.Fatalf("Unable to get comments on PR %v: %v", pr.GetNumber(), err)
		}
		for _, comment := range c {
			if contains(users, comment.GetUser().GetLogin()) {
				return true
			}
		}
		page = r.NextPage
		if page == 0 {
			return false
		}
	}
}

func retryListCommentsUpTo(count int, f func() ([]*github.IssueComment, *github.Response, error)) ([]*github.IssueComment, *github.Response, error) {
	i := 1
	for {
		c, r, err := f()
		time.Sleep(sleep)
		if err == nil {
			return c, r, nil
		} else if i > count {
			return c, r, err
		}
		i++
	}
}

func prReviewedBy(client *github.Client, pr *github.PullRequest, users []string) bool {
	page := 0
	for {
		c, r, err := retryListReviewsUpTo(retryCount, func() ([]*github.PullRequestReview, *github.Response, error) {
			return client.PullRequests.ListReviews(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.ListOptions{
				Page: page,
			})
		})
		if err != nil {
			log.Fatalf("Unable to get reviews on PR %v: %v", pr.GetNumber(), err)
		}
		for _, comment := range c {
			if contains(users, comment.GetUser().GetLogin()) {
				return true
			}
		}
		page = r.NextPage
		if page == 0 {
			return false
		}
	}
}

func retryListReviewsUpTo(count int, f func() ([]*github.PullRequestReview, *github.Response, error)) ([]*github.PullRequestReview, *github.Response, error) {
	i := 1
	for {
		c, r, err := f()
		time.Sleep(sleep)
		if err == nil {
			return c, r, nil
		} else if i > count {
			return c, r, err
		}
		i++
	}
}

type lineCounter struct {
	client    *github.Client
	cache     map[string]int64
	cacheLock sync.Mutex
}

func (lc *lineCounter) added(prs []*github.PullRequest) int64 {
	input := make(chan *github.PullRequest, len(prs))
	output := make(chan int64, len(prs))
	for i := 0; i < parallelWorkers; i++ {
		go func() {
			for {
				pr := <-input
				output <- lc.countNonVendorLines(pr)
			}
		}()
	}
	for _, pr := range prs {
		input <- pr
	}
	var count int64
	for range prs {
		count += <-output
	}
	return count
}

func (lc *lineCounter) countNonVendorLines(pr *github.PullRequest) int64 {
	count := func() int64 {
		lc.cacheLock.Lock()
		defer lc.cacheLock.Unlock()
		if count, contains := lc.cache[pr.GetHTMLURL()]; contains {
			return count
		}
		return -1
	}()
	if count != -1 {
		return count
	}
	count = 0
	page := 0
	for {
		f, r, err := retryListFilesUpTo(retryCount, func() ([]*github.CommitFile, *github.Response, error) {

			return lc.client.PullRequests.ListFiles(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.ListOptions{
				Page: page,
			})
		})

		if err != nil {
			log.Fatalf("Unable to get files on PR %v: %v", pr.GetNumber(), err)
		}
		for _, file := range f {
			if !strings.HasPrefix(file.GetFilename(), "vendor/") && !strings.Contains(file.GetFilename(), "/vendor/") {
				count += int64(file.GetAdditions())
			}
		}
		page = r.NextPage
		if page == 0 {
			break
		}
	}
	lc.cacheLock.Lock()
	defer lc.cacheLock.Unlock()
	lc.cache[pr.GetHTMLURL()] = count
	return count
}

func retryListFilesUpTo(count int, f func() ([]*github.CommitFile, *github.Response, error)) ([]*github.CommitFile, *github.Response, error) {
	i := 1
	for {
		c, r, err := f()
		time.Sleep(sleep)
		if err == nil {
			return c, r, nil
		} else if i > count {
			return c, r, err
		}
		i++
	}
}
