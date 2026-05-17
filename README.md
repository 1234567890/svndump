# svndump

downloads pristine files from an exposed `.svn` directory using the working copy's SQLite database.

### usage

`./svndump -d wc-downloaded.db -u https://example.com/ -H "X-Header: here" -o folder`

or, assuming wc.db is in the current directory:

`./svndump -u https://example.com/ -H "X-Header: here" -o folder`