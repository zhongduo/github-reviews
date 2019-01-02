package main

import (
	"context"
	"flag"
	"github.com/google/go-github/github"
	"golang.org/x/oauth2"
	"io/ioutil"
	"log"
	"net/http"
	"strings"
	"time"
)

const (
	timeFormat = "1-2-2006"
)

var (
	tokenFile = flag.String("token_file", "", "Path to the token file")
	owner = flag.String("owner", "knative", "GitHub user name")
	start     = flag.String("start", time.Now().Format(timeFormat), "Start date in '%m-%d-%y' format")
	end = flag.String("end", time.Now().Format(timeFormat), "End date in %m-%d-%y format")
	repos stringSlice
	users stringSlice
	parallelWorkers = 3
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
	prs := listPRs(client)
	log.Printf("Finished listing PRs. %v", len(prs))

	ftpr := filterPRsForTime(prs, startTime, endTime)
	log.Printf("Finished filtering PRs for time. %v", len(ftpr))

	fapr := filterPRsForAuthors(ftpr, users)
	log.Printf("Finished filtering PRs for authors. %v", len(fapr))

	touchedPRs := filterPRsForTouch(client, fapr, users)
	log.Printf("Total PRs: %v. Commented PRs: %v", len(fapr), len(touchedPRs))

	totalLinesAdded := countLinesAdded(client, fapr)
	touchedLinesAdded := countLinesAdded(client, touchedPRs)
	log.Printf("Total lines added: %v. I reviewed %v.", totalLinesAdded, touchedLinesAdded)
	if totalLinesAdded > 0 {
		log.Printf("Percent lines reviewed: %v", float64(touchedLinesAdded)/float64(totalLinesAdded))
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

func listPRs(client *github.Client) []*github.PullRequest {
	prs := make([]*github.PullRequest, 0)
	for _, repo := range repos {
		page := 0
		for {
			p, r, err := client.PullRequests.List(context.TODO(), *owner, repo, &github.PullRequestListOptions{
				State:     "all",
				Sort:      "updated",
				Direction: "desc",
				ListOptions: github.ListOptions{
					Page: page,
				},
			})
			if err != nil {
				log.Fatalf("Unable to list PRs for page: %v: %v", page, err)
			}
			prs = append(prs, p...)
			page = r.NextPage
			if page == 0 {
				break
			}
		}
	}
	return prs
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

func filterPRsForAuthors(unfiltered []*github.PullRequest, authors []string) []*github.PullRequest {
	prs := make([]*github.PullRequest, 0)
	for _, pr := range unfiltered {
		if !contains(authors, pr.GetUser().GetLogin()) {
			prs = append(prs, pr)
		}
	}
	return prs
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
		c, r, err := client.Issues.ListComments(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.IssueListCommentsOptions{
			ListOptions: github.ListOptions{
				Page: page,
			},
		})
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

func prReviewedBy(client *github.Client, pr *github.PullRequest, users []string) bool {
	page := 0
	for {
		c, r, err := client.PullRequests.ListReviews(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.ListOptions{
			Page: page,
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

func countLinesAdded(client *github.Client, prs []*github.PullRequest) int64 {
	input := make(chan *github.PullRequest, len(prs))
	output := make(chan int64, len(prs))
	for i := 0; i < parallelWorkers; i++ {
		go func() {
			for {
				pr := <-input
				output <- countNonVendorLines(client, pr)
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

func countNonVendorLines(client *github.Client, pr *github.PullRequest) int64 {
	var count int64
	page := 0
	for {
		f, r, err := client.PullRequests.ListFiles(context.TODO(), pr.GetBase().GetRepo().GetOwner().GetLogin(), pr.GetBase().GetRepo().GetName(), pr.GetNumber(), &github.ListOptions{
			Page: page,
		})

		if err != nil {
			log.Fatalf("Unable to get reviews on PR %v: %v", pr.GetNumber(), err)
		}
		for _, file := range f {
			if !strings.HasPrefix(file.GetFilename(), "vendor/") {
				count += int64(file.GetAdditions())
			}
		}
		page = r.NextPage
		if page == 0 {
			return count
		}
	}
}
