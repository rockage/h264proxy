import Foundation
import Darwin

struct TunnelConfiguration: Codable {
    var endpoint: String
    var serverIPv4: String {
        endpoint.split(separator: ":", omittingEmptySubsequences: false).first.map(String.init) ?? ""
    }
    func validate() throws {
        let parts = endpoint.split(separator: ":", omittingEmptySubsequences: false)
        var address = in_addr()
        guard parts.count == 2, inet_pton(AF_INET, String(parts[0]), &address) == 1,
              let port = Int(parts[1]), (1...65535).contains(port) else {
            throw TunnelError("服务器地址须为 IPv4:端口，例如 207.148.110.180:9300。")
        }
    }
    var engineJSON: String { String(data: try! JSONEncoder().encode(self), encoding: .utf8)! }
}

struct TunnelError: LocalizedError {
    let message: String
    init(_ message: String) { self.message = message }
    var errorDescription: String? { message }
}

enum BuildConfiguration {
    static var keychainGroup: String { Bundle.main.object(forInfoDictionaryKey: "H264KeychainGroup") as? String ?? "UNSIGNED" }
    static var isUnsigned: Bool { keychainGroup.hasPrefix("UNSIGNED") || keychainGroup.contains("$(") }
    static var providerBundleIdentifier: String { "com.roca.h264proxy.mac.PacketTunnel" }
}
