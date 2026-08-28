#!/usr/bin/env swift
import Foundation
import Security

func fail(_ message: String) -> Never {
    FileHandle.standardError.write(Data(("ERROR: \(message)\n").utf8))
    exit(1)
}

func itemQuery(service: String) -> [String: Any] {
    [
        kSecClass as String: kSecClassGenericPassword,
        kSecAttrService as String: service,
        kSecAttrAccount as String: NSUserName(),
    ]
}

let arguments = CommandLine.arguments
guard arguments.count >= 3 else {
    fail("usage: keychain-signing-key.swift read SERVICE | store PRIVATE_PEM SERVICE")
}

let command = arguments[1]
switch command {
case "read":
    guard arguments.count == 3 else {
        fail("usage: keychain-signing-key.swift read SERVICE")
    }
    let service = arguments[2]
    var query = itemQuery(service: service)
    query[kSecMatchLimit as String] = kSecMatchLimitOne
    query[kSecReturnData as String] = true
    var result: CFTypeRef?
    let status = SecItemCopyMatching(query as CFDictionary, &result)
    guard status == errSecSuccess, let value = result as? Data, !value.isEmpty else {
        fail("Keychain signing-key item is unavailable")
    }
    FileHandle.standardOutput.write(value)

case "store":
    guard arguments.count == 4 else {
        fail("usage: keychain-signing-key.swift store PRIVATE_PEM SERVICE")
    }
    let keyURL = URL(fileURLWithPath: arguments[2])
    let service = arguments[3]
    let keyData: Data
    do {
        keyData = try Data(contentsOf: keyURL)
    } catch {
        fail("private PEM file is unavailable")
    }
    guard !keyData.isEmpty else {
        fail("refusing to store an empty Keychain value")
    }
    var query = itemQuery(service: service)
    query[kSecValueData as String] = keyData
    query[kSecAttrAccessible as String] = kSecAttrAccessibleWhenUnlockedThisDeviceOnly
    let status = SecItemAdd(query as CFDictionary, nil)
    guard status == errSecSuccess else {
        fail("refusing to replace or create Keychain signing-key item (status \(status))")
    }

default:
    fail("unknown command: \(command)")
}
