import SwiftUI
import NetworkExtension
import UniformTypeIdentifiers

@main
@MainActor
struct H264ProxyApp: App {
    @StateObject private var model = TunnelModel()
    var body: some Scene {
        WindowGroup { ContentView(model: model).frame(minWidth: 540, minHeight: 390) }
            .windowResizability(.contentSize)
    }
}

@MainActor
final class TunnelModel: ObservableObject {
    @Published var endpoint = "207.148.110.180:9300"
    @Published var keyInput = ""
    @Published var status = NEVPNStatus.disconnected
    @Published var message = ""
    @Published var busy = false
    @Published var hasSavedKey = false
    @Published var statistics = ""
    private var manager: NETunnelProviderManager?
    private var observer: NSObjectProtocol?

    init() {
        observer = NotificationCenter.default.addObserver(forName: .NEVPNStatusDidChange, object: nil, queue: .main) { [weak self] _ in
            Task { @MainActor in self?.refresh() }
        }
        guard !BuildConfiguration.isUnsigned else { return }
        NETunnelProviderManager.loadAllFromPreferences { [weak self] managers, error in
            Task { @MainActor in
                guard let self else { return }
                if let error { self.message = error.localizedDescription; return }
                self.manager = managers?.first { ($0.protocolConfiguration as? NETunnelProviderProtocol)?.providerBundleIdentifier == BuildConfiguration.providerBundleIdentifier }
                if let configuration = self.manager?.protocolConfiguration as? NETunnelProviderProtocol {
                    self.endpoint = configuration.providerConfiguration?["endpoint"] as? String ?? self.endpoint
                    self.hasSavedKey = configuration.passwordReference != nil
                }
                self.refresh()
            }
        }
    }
    var statusText: String {
        switch status {
        case .connected: return "已连接"
        case .connecting: return "正在连接"
        case .disconnecting: return "正在断开"
        case .reasserting: return "正在恢复连接"
        case .invalid: return "尚未配置"
        default: return "未连接"
        }
    }
    var editable: Bool { !busy && status != .connected && status != .connecting && status != .disconnecting }
    func refresh() {
        status = manager?.connection.status ?? .disconnected
        if status == .connected, let session = manager?.connection as? NETunnelProviderSession {
            try? session.sendProviderMessage(Data("status".utf8)) { [weak self] data in
                guard let data, let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any] else { return }
                let input = object["input_packets"] as? Int ?? 0
                let output = object["output_packets"] as? Int ?? 0
                Task { @MainActor in self?.statistics = "虚拟接口：接收 \(input) 包，发送 \(output) 包" }
            }
        } else { statistics = "" }
    }
    func connect() {
        guard editable else { return }
        do {
            guard !BuildConfiguration.isUnsigned else { throw TunnelError("未签名构建可查看界面，但不能安装或启动系统 VPN。") }
            let configuration = TunnelConfiguration(endpoint: endpoint.trimmingCharacters(in: .whitespacesAndNewlines))
            try configuration.validate()
            let old = manager?.protocolConfiguration as? NETunnelProviderProtocol
            let reference: Data
            if !keyInput.isEmpty {
                guard let key = Data(base64Encoded: keyInput.trimmingCharacters(in: .whitespacesAndNewlines)), key.count == 32 else {
                    throw TunnelError("请输入 32 字节共享密钥的 Base64，或导入现有密钥文件。")
                }
                reference = try SharedKeychain.save(key)
            } else if let saved = old?.passwordReference { reference = saved }
            else { throw TunnelError("请先导入共享密钥。") }
            let protocolSettings = NETunnelProviderProtocol()
            protocolSettings.providerBundleIdentifier = BuildConfiguration.providerBundleIdentifier
            protocolSettings.serverAddress = configuration.endpoint
            protocolSettings.providerConfiguration = ["endpoint": configuration.endpoint, "schema": 1]
            protocolSettings.passwordReference = reference
            protocolSettings.disconnectOnSleep = true
            let tunnel = manager ?? NETunnelProviderManager()
            tunnel.protocolConfiguration = protocolSettings
            tunnel.localizedDescription = "H264Proxy"
            tunnel.isEnabled = true
            busy = true; message = ""; manager = tunnel
            tunnel.saveToPreferences { [weak self] error in
                if let error { Task { @MainActor in self?.failed(error) }; return }
                tunnel.loadFromPreferences { error in
                    Task { @MainActor in
                        guard let self else { return }
                        if let error { self.failed(error); return }
                        do {
                            guard let session = tunnel.connection as? NETunnelProviderSession else { throw TunnelError("系统未创建 Packet Tunnel 会话。") }
                            try session.startTunnel()
                            self.busy = false; self.keyInput = ""; self.hasSavedKey = true; self.refresh()
                        } catch { self.failed(error) }
                    }
                }
            }
        } catch { failed(error) }
    }
    func disconnect() { manager?.connection.stopVPNTunnel(); refresh() }
    private func failed(_ error: Error) { busy = false; message = error.localizedDescription; refresh() }
    func importKey(_ url: URL) {
        let scoped = url.startAccessingSecurityScopedResource()
        defer { if scoped { url.stopAccessingSecurityScopedResource() } }
        do {
            let data = try Data(contentsOf: url)
            if data.count == 32 { keyInput = data.base64EncodedString() }
            else if let text = String(data: data, encoding: .utf8), let decoded = Data(base64Encoded: text.trimmingCharacters(in: .whitespacesAndNewlines)), decoded.count == 32 {
                keyInput = decoded.base64EncodedString()
            } else { throw TunnelError("文件须包含 32 字节原始密钥或对应的 Base64。") }
            message = "密钥已导入，连接时将写入共享钥匙串。"
        } catch { failed(error) }
    }
}

@MainActor
struct ContentView: View {
    @ObservedObject var model: TunnelModel
    @State private var importing = false
    var body: some View {
        VStack(alignment: .leading, spacing: 18) {
            HStack {
                Image(systemName: "network").font(.largeTitle)
                VStack(alignment: .leading) {
                    Text("H264Proxy").font(.title2.bold())
                    Text("全局 TCP 代理 · WebRTC / ICE-TCP").foregroundStyle(.secondary)
                }
                Spacer()
                Text(model.statusText).font(.headline)
            }
            if BuildConfiguration.isUnsigned {
                Text("未签名开发构建：可查看配置界面；启用系统 VPN 需签名和 Network Extension 配置文件。")
                    .padding(10).background(.orange.opacity(0.12)).clipShape(RoundedRectangle(cornerRadius: 8))
            }
            Form {
                TextField("服务器 IPv4:端口", text: $model.endpoint).disabled(!model.editable)
                HStack {
                    SecureField(model.hasSavedKey ? "已有钥匙串密钥；留空继续使用" : "共享密钥（Base64）", text: $model.keyInput)
                    Button("导入密钥…") { importing = true }
                }.disabled(!model.editable)
            }
            Text("系统管理虚拟接口、路由和 DNS。此阶段仅转发 TCP 与隧道 DNS，通用 UDP/QUIC 暂不支持。")
                .font(.callout).foregroundStyle(.secondary)
            if !model.message.isEmpty { Text(model.message).font(.callout).textSelection(.enabled) }
            HStack {
                Button("连接") { model.connect() }
                    .keyboardShortcut(.defaultAction)
                    .disabled(!model.editable || BuildConfiguration.isUnsigned)
                Button("断开") { model.disconnect() }
                    .disabled(model.status != .connected && model.status != .connecting && model.status != .reasserting)
                Spacer()
                Button("刷新状态") { model.refresh() }
            }
            if !model.statistics.isEmpty { Text(model.statistics).font(.caption.monospaced()).textSelection(.enabled) }
        }.padding(24)
            .fileImporter(isPresented: $importing, allowedContentTypes: [.data]) { result in
                switch result { case .success(let url): model.importKey(url); case .failure(let error): model.message = error.localizedDescription }
            }
    }
}
