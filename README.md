## Instructions

1. Download a GitHub [Personal Access Token](https://help.github.com/articles/creating-a-personal-access-token-for-the-command-line/) to a file locally, `$HOME/github.oauth` is the file that will be used in these instructions.
1. Run the tool.
    ```shell
    go run main.go --token_file=$HOME/github.oauth --owner=knative --repos=eventing --repos=eventing-sources --users=adamharwayne --users=Harwayne --start=08-31-2018 --end=01-01-2019    
    ```
