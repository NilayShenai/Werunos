.PHONY: build test clean app

build:
	go build -o werunos .

test:
	go test ./...

clean:
	rm -f werunos
	rm -rf Werunos.app
	rm -f app/AppIcon.icns

app: build
	@if [ ! -f app/AppIcon.icns ]; then ./app/generate_icons.sh; fi
	swiftc -O -sdk $$(xcrun --show-sdk-path) -o WerunosApp app/main.swift
	rm -rf Werunos.app
	mkdir -p Werunos.app/Contents/MacOS
	mkdir -p Werunos.app/Contents/Resources
	mv WerunosApp Werunos.app/Contents/MacOS/
	cp werunos Werunos.app/Contents/MacOS/
	cp app/Info.plist Werunos.app/Contents/Info.plist
	cp app/AppIcon.icns Werunos.app/Contents/Resources/
	echo -n "APPL????" > Werunos.app/Contents/PkgInfo
	/System/Library/Frameworks/CoreServices.framework/Frameworks/LaunchServices.framework/Support/lsregister -f Werunos.app
	@echo "Werunos.app successfully built and registered!"
