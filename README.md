# tutu

`tutu` is the translation of tube or pipe in euskera: https://hiztegiak.elhuyar.eus/eu/tutu

`tutu` creates pipes between data streams: network connections, files, executed commands, etc.

## Install

Installation using go:

```
go get github.com/igolaizola/tutu/cmd/tutu
```

## Usage

```
tutu [global_options] [[stream_options]...]
```

Each stream option list must start with `-<N> <type>` where `N` is an integer value specifying the stream number and `type` is the stream type.

See all available parameters calling `help`:

```
tutu -help
```

## Examples

Example to pipe the standard input and output with an UDP connection:

```
tutu -1 stdio -2 udp -remote localhost:5050
```

This same example can be launched using the following config file:

```
# myconf.conf

# standard input/output stream
1 stdio

# UDP stream
2 udp
remote localhost:5050
```

```
tutu -config myconf.conf
```

