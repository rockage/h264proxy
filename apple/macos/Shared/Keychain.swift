import Foundation
import Security

enum SharedKeychain {
    private static var base: [String: Any] {
        [kSecClass as String: kSecClassGenericPassword,
         kSecAttrService as String: "com.roca.h264proxy.shared-key",
         kSecAttrAccount as String: "gateway",
         kSecAttrAccessGroup as String: BuildConfiguration.keychainGroup]
    }
    static func save(_ key: Data) throws -> Data {
        guard key.count == 32 else { throw TunnelError("共享密钥必须为 32 字节。") }
        guard !BuildConfiguration.isUnsigned else { throw TunnelError("此构建未签名，尚不能使用共享钥匙串或启动系统 VPN。") }
        var item = base
        item[kSecValueData as String] = key
        item[kSecAttrAccessible as String] = kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly
        let status = SecItemAdd(item as CFDictionary, nil)
        if status == errSecDuplicateItem {
            let update = SecItemUpdate(base as CFDictionary, [kSecValueData as String: key] as CFDictionary)
            guard update == errSecSuccess else { throw failure(update) }
        } else if status != errSecSuccess { throw failure(status) }
        var query = base
        query[kSecReturnPersistentRef as String] = true
        var result: CFTypeRef?
        let read = SecItemCopyMatching(query as CFDictionary, &result)
        guard read == errSecSuccess, let reference = result as? Data else { throw failure(read) }
        return reference
    }
    static func read(reference: Data) throws -> Data {
        let query: [String: Any] = [kSecValuePersistentRef as String: reference, kSecReturnData as String: true,
                                    kSecMatchLimit as String: kSecMatchLimitOne]
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        guard status == errSecSuccess, let key = result as? Data, key.count == 32 else { throw failure(status) }
        return key
    }
    private static func failure(_ status: OSStatus) -> Error {
        TunnelError("无法访问共享钥匙串（\(status)）；请检查 App 和扩展的签名、配置文件及 Keychain Access Group。")
    }
}
