# Caddy Pixbooster
On-the-fly image conversion for maximal network performances

**DO NOT USE IN PRODUCTION!**

This work is at its very early stages and should only be used for testing. 

## Desription

Pixbooster does two things:

1. it adds modern files format sources (WebP, AVIF and JXL) to the HTML,
2. it serve those files by converting the orignal ones on the fly, from saved files after.

### How `<img>` is handled

When Pixbooster meet a `<img>` tag out of any `picture` tag, it wrap this `<img>` in a new `<picture>` and add `<source>` tags for each modern formats.

So:
```html
<img src="test.jpg" style="width: 100px" title="test" alt="alt">
```

became:
```html
<picture style="width: 100px" title="test"><source srcset="test.jpg.pixbooster.jxl" type="image/jxl"/><source srcset="test.jpg.pixbooster.avif" type="image/avif"/><source srcset="test.jpg.pixbooster.webp" type="image/webp"/><img src="test.jpg" style="width: 100px" title="test" alt="alt"/></picture>
```

The `pixbooster` is afterward used by Pixbooster to know which files it have to generate.

### How `<picture>` is handled

When Pixbooster met a `<picture>`, it generate a new `<source>` for each `<source>` it contains and for each modern formats.

So:
```html
<picture>
    <source srcset="test3.png" type="image/png"  media="(min-width: 800px)">
    <img src="test2.png" style="width: 100px" title="test" alt="alt">
</picture>
```

became:
```html
<picture>
    <source media="(min-width: 800px)" srcset="test3.png.pixbooster.jxl" type="image/jxl"/><source media="(min-width: 800px)" srcset="test3.png.pixbooster.avif" type="image/avif"/><source media="(min-width: 800px)" srcset="test3.png.pixbooster.webp" type="image/webp"/><source srcset="test3.png" type="image/png" media="(min-width: 800px)"/>
    <img src="test2.png" style="width: 100px" title="test" alt="alt"/>
</picture>
```

## How to test

### Clone

```sh
$ git clone https://github.com/PixyBlue/caddy-pixbooster.git
$ cd caddy-pixbooster
```

### Configure

Here is a Caddyfile sample for tests:

```
{
    debug
}

http://localhost:8080 {
    root ./public
    route {
        pixbooster
    }
    file_server
}
```

`pixbooster` accept some options to disable some image formats to be treaten or to be produce:
- `nojpg`, `nopng`, `nowebpinput` make Pixbooster ignore respectively JPEG, PNG and WebP files in the incomming HTML,
- `nowebpouput`, `noavif`, `nojxl` disable adding sources respectively for WebP, AVIF and JXL formats in the incomming HTML and avoid the conversion of such files from request URL.

### Provide some contents

```sh
$ mkdir public
$ echo '<!DOCTYPE html>
<html>

<head>
    <title>Foo</title>
</head>

<body>
    <img src="test.jpg" style="width: 100px" title="test" alt="alt">
    <img src="test5.png" srcset="test6.png 2x, test7.png 1x" style="width: 100px" title="test" alt="alt">
    <picture>
        <source srcset="test3.png" type="image/png" media="(min-width: 800px)">
        <img src="test2.png" style="width: 100px" title="test" alt="alt">
    </picture>
</body>

</html>' > public/index.html
```

Add needed picture files to `public`.

### Build & Run

```sh
$ CGO_ENABLED=1 xcaddy run --config Caddyfile #CGO is required to enable convertion to the WebP format
```

### See the magic

```sh
$ curl http://localhost:8080
```

## Detailed configuration
### Syntax
```
pixbooster [nowebpoutput|noavif|nojxl|nojpg|nopng] {
	[nowebpoutput|noavif|nojxl|nojpg|nopng]
	quality <integer between 0 and 100>
    storage <path where to store optimized files>
	webp {
		quality <integer between 0 and 100>
		lossless
		exact
	}
	avif {
		quality <integer between 0 and 100>
		qualityalpha <integer between 0 and 100>
		speed <integer between 0 and 10>
	}
	jxl {
		quality <integer between 0 and 100>
		effort <integer between 0 and 10>
	}
}
```
Pixbooster must be enabled in a `route` directive.

### Samples
The Caddfyfile configuration enable you to access to all options offered by the libraries we use. Here is a complete sample:

```
http://localhost:8080 {
    route {
        pixbooster {
            nowebpoutput
            nojxl
            avif {
                quality 65
                speed 7
            }
        }
    }
}
```
## TODO ?
- [ ] Provide [JXL polyfill](https://github.com/niutech/jxl.js)
- [ ] Add `data-pixbooster-quality` html attribute to force quality setting on individual picture
- [ ] Handle CSS
- [ ] Completly avoid CGO to support WebP output
