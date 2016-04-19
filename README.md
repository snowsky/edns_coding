# Hello EDNS

----

# Dependencies
```
go get gopkg.in/yaml.v2
go get github.com/codegangsta/cli
go get github.com/snowsky/dns
```

# Configuration

```yaml
zones:
  coding.net:
    coding.net:
      a:
        - 1.1.1.1
        - 2.2.2.2
      mx:
        - 10 1.1.1.1
        - 20 2.2.2.2
    git.coding.net:
      proxy:
        - 125.39.1.118
        - 14.215.100.33
        - 222.186.132.179
        - 111.202.74.158
```

The config file starts with "zones" keyword. The structure is zone name -> host name -> records.


# Start Service
```
go run edns_coding.go start --conf dns.yml
```
