#!/bin/sh
# Builds TermaSensor.app with its two system extensions into ./build. Without SIGN_ID it
# signs ad hoc, which only a Mac with SIP and AMFI off will run (the spike's VM); with a
# Developer ID identity and its provisioning profiles it is the shippable build.
#   macos/TermaSensor/build.sh            # ad hoc
#   SIGN_ID="Developer ID Application: …" macos/TermaSensor/build.sh
set -eu
cd "$(dirname "$0")"
APP_ID=${APP_ID:-org.mirador.terma.sensor}
SIGN_ID=${SIGN_ID:--}
OUT=build/TermaSensor.app
NE=$OUT/Contents/Library/SystemExtensions/$APP_ID.network.systemextension
ES=$OUT/Contents/Library/SystemExtensions/$APP_ID.security.systemextension
TARGET=arm64-apple-macos15.0
rm -rf build && mkdir -p "$OUT/Contents/MacOS" "$NE/Contents/MacOS" "$ES/Contents/MacOS"

swiftc -O -target $TARGET -module-name TermaSensor -o "$OUT/Contents/MacOS/TermaSensor" \
  App/main.swift -framework SystemExtensions -framework NetworkExtension
swiftc -O -target $TARGET -module-name TermaSensorNetwork -o "$NE/Contents/MacOS/$APP_ID.network" \
  Shared/Link.swift NetExt/FilterDataProvider.swift NetExt/main.swift -framework NetworkExtension -lbsm
swiftc -O -target $TARGET -module-name TermaSensorSecurity -o "$ES/Contents/MacOS/$APP_ID.security" \
  Shared/Link.swift SecExt/main.swift -lEndpointSecurity -lbsm

plist() { # file, then key/value XML lines
  f=$1; shift
  { echo '<?xml version="1.0" encoding="UTF-8"?>'
    echo '<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">'
    echo '<plist version="1.0"><dict>'; printf '%s\n' "$@"; echo '</dict></plist>'; } > "$f"
}
common() { # id, executable, package type
  printf '%s\n' "<key>CFBundleIdentifier</key><string>$1</string>" \
    "<key>CFBundleExecutable</key><string>$2</string>" "<key>CFBundleName</key><string>$2</string>" \
    "<key>CFBundlePackageType</key><string>$3</string>" "<key>CFBundleShortVersionString</key><string>0.1.0</string>" \
    "<key>CFBundleVersion</key><string>${BUILD_NUMBER:-10}</string>" "<key>LSMinimumSystemVersion</key><string>15.0</string>" \
    "<key>CFBundleInfoDictionaryVersion</key><string>6.0</string>"
}
plist "$OUT/Contents/Info.plist" "$(common "$APP_ID" TermaSensor APPL)" "<key>LSUIElement</key><true/>"
plist "$NE/Contents/Info.plist" "$(common "$APP_ID.network" "$APP_ID.network" SYSX)" \
  "<key>NSSystemExtensionUsageDescription</key><string>Counts the connections of coding agent sessions you opted in to.</string>" \
  "<key>NetworkExtension</key><dict><key>NEMachServiceName</key><string>$APP_ID.network</string>" \
  "<key>NEProviderClasses</key><dict><key>com.apple.networkextension.filter-data</key><string>FilterDataProvider</string></dict></dict>"
plist "$ES/Contents/Info.plist" "$(common "$APP_ID.security" "$APP_ID.security" SYSX)" \
  "<key>NSSystemExtensionUsageDescription</key><string>Follows the processes and files of coding agent sessions you opted in to.</string>" \
  "<key>NSEndpointSecurityEarlyBoot</key><false/>" \
  "<key>NSEndpointSecurityMachServiceName</key><string>$APP_ID.security</string>"

ent() { # file, then key/value XML lines
  plist "$@"
}
# The Developer ID spelling of the filter entitlement; App Store and development builds
# drop the -systemextension suffix.
FILTER='<key>com.apple.developer.networking.networkextension</key><array><string>content-filter-provider-systemextension</string></array>'
ent build/app.entitlements "$FILTER" '<key>com.apple.developer.system-extension.install</key><true/>'
ent build/network.entitlements "$FILTER" '<key>com.apple.security.app-sandbox</key><true/>' \
  '<key>com.apple.security.network.client</key><true/>' \
  "<key>com.apple.security.application-groups</key><array><string>$APP_ID</string></array>"
ent build/security.entitlements '<key>com.apple.developer.endpoint-security.client</key><true/>'

# An ad-hoc signature's designated requirement is its own hash, so every build would fail
# the requirement the filter configuration recorded for the last one; give it the bundle
# identifier instead, which a Developer ID build pairs with its team.
sign() {
  if [ "$SIGN_ID" = - ]; then
    codesign --force --options runtime --timestamp=none -s - --entitlements "$1" -r="designated => identifier \"$3\"" "$2"
  else
    codesign --force --options runtime --timestamp -s "$SIGN_ID" --entitlements "$1" "$2"
  fi
}
sign build/network.entitlements "$NE" "$APP_ID.network"
sign build/security.entitlements "$ES" "$APP_ID.security"
sign build/app.entitlements "$OUT" "$APP_ID"
codesign --verify --deep --strict "$OUT" && echo "built $OUT (signed: $SIGN_ID)"
