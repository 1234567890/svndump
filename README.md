# svndump

downloads pristine files from an exposed `.svn` directory using the working copy's SQLite database.

### usage

- `-H "User-Agent: Mozilla/5.0"`: headers to add to each request (can be used multiple times)
- `-d wc-downloaded.db`: path to wc.db (omit to auto-download from the target)
- `-o folder`: output directory for downloaded files (defaults to target hostname when -d is omitted) (default ".")
- `-r 10`: maximum number of retry attempts per file (default 5)
- `-t 20`: number of concurrent download workers (default 10)
- `-u https://example.com/`: base URL of the target site

example (automatically downloads wc.db, requires a custom header, outputs to a non-default folder):

`./svndump -u https://example.com/ -H "X-Header: here" -o folder`
