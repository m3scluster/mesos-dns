#Dockerfile vars

#vars
PROJECTNAME=mesos-dns
DESCRIPTION=DNS Based service discovery for mesos 
UNAME_M=`uname -m`
IMAGENAME=${PROJECTNAME}
REPO=avhost
TAG=$(shell git describe)
BRANCH=`git rev-parse --abbrev-ref HEAD`
BUILDDATE=`date -u +%Y-%m-%dT%H:%M:%SZ`
LICENSE=Apache-2.0 license
VERSION_TU=$(subst -, ,$(TAG:v%=%))
BUILD_VERSION = $(word 1,$(VERSION_TU))
IMAGEFULLNAME=${REPO}/${IMAGENAME}
LASTCOMMIT=$(shell git log -1 --pretty=short | tail -n 1 | tr -d " " | tr -d "UPDATE:")

FPM_OPTS= -s dir -n $(PROJECTNAME) -v $(BUILD_VERSION) \
	--architecture $(UNAME_M) \
	--url "https://www.aventer.biz" \
	--license "$(LICENSE)" \
	--description "$(DESCRIPTION)" \
	--maintainer "AVENTER Support <support@aventer.biz>" \
	--vendor "AVENTER UG (haftungsbeschraenkt)"

CONTENTS= usr/bin etc usr/lib

help:
	    @echo "Makefile arguments:"
	    @echo ""
	    @echo "Makefile commands:"
	    @echo "build"
	    @echo "all"
		@echo ${TAG}

.DEFAULT_GOAL := all

ifeq (${BRANCH}, master) 
        BRANCH=latest
endif

ifneq ($(shell echo $(LASTCOMMIT) | grep -E '^v([0-9]+\.){0,2}(\*|[0-9]+)'),)
        BRANCH=${LASTCOMMIT}
else
        BRANCH=latest
endif


build-bin:
	@echo ">>>> Build binary"
	@CGO_ENABLED=0 GOOS=linux go build -o build/$(PROJECTNAME) -a -installsuffix cgo -ldflags "-X main.BuildVersion=${BUILDDATE} -X main.GitVersion=${TAG} -extldflags \"-static\"" .

build-docker:
	@echo ">>>> Build Docker: latest"
	@docker build --build-arg TAG=${TAG} --build-arg BUILDDATE=${BUILDDATE} -t ${IMAGEFULLNAME}:latest .

push:
	@echo ">>>> Publish docker image: " ${BRANCH}
	@docker buildx create --use --name buildkit
	@docker buildx build --platform linux/amd64,linux/arm64 --push --build-arg TAG=${TAG} --build-arg BUILDDATE=${BUILDDATE} -t ${IMAGEFULLNAME}:latest .
	@docker buildx build --platform linux/amd64,linux/arm64 --push --build-arg TAG=${TAG} --build-arg BUILDDATE=${BUILDDATE} -t ${IMAGEFULLNAME}:${BRANCH} .
	@docker buildx rm buildkit

deb: build-dns
	@echo ">>>> Build DEB"
	@mkdir -p /tmp/toor/usr/bin
	@mkdir -p /tmp/toor/etc/$(PROJECTNAME)
	@mkdir -p /tmp/toor/usr/lib/systemd/system
	@cp build/$(PROJECTNAME) /tmp/toor/usr/bin
	@cp build/$(PROJECTNAME).service /tmp/toor/usr/lib/systemd/system
	@cp config.json.sample /tmp/toor/etc/$(PROJECTNAME)/config.json.sample
	@cp LICENSE /tmp/toor/license
	@fpm -t deb -C /tmp/toor/ --config-files etc $(FPM_OPTS) $(CONTENTS)

rpm: build-dns
	@echo ">>>> Build RPM"
	@mkdir -p /tmp/toor/usr/bin
	@mkdir -p /tmp/toor/etc/$(PROJECTNAME)
	@mkdir -p /tmp/toor/usr/lib/systemd/system
	@cp build/$(PROJECTNAME) /tmp/toor/usr/bin
	@cp build/$(PROJECTNAME).service /tmp/toor/usr/lib/systemd/system
	@cp config.json.sample /tmp/toor/etc/$(PROJECTNAME)/config.json.sample
	@cp LICENSE /tmp/toor/license
	@fpm -t rpm -C /tmp/toor/ --config-files etc $(FPM_OPTS) $(CONTENTS)

sboom:
	syft dir:. > sbom.txt
	syft dir:. -o json > sbom.json

seccheck:
	grype --add-cpes-if-none .

imagecheck:
	trivy image ${IMAGEFULLNAME}:latest

update-gomod:
	go mod -u

check: sboom seccheck
all: check build-docker imagecheck
