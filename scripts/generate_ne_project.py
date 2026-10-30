#!/usr/bin/env python3
"""Generate the two-target Xcode project using only the Python standard library."""
import hashlib
import json
from pathlib import Path

root = Path(__file__).resolve().parents[1]
project = root / 'apple/macos/H264Proxy.xcodeproj'
objects = {}

def ident(name):
    return hashlib.sha1(name.encode()).hexdigest()[:24].upper()

def add(label, isa, **fields):
    key = ident(label)
    objects[key] = {'isa': isa, **fields}
    return key

def file(path, kind='sourcecode.swift', tree='SOURCE_ROOT'):
    return add('file:' + path, 'PBXFileReference', lastKnownFileType=kind, path=path, sourceTree=tree)

def build(name, reference, **settings):
    values = {'fileRef': reference}
    if settings:
        values['settings'] = settings
    return add('build:' + name, 'PBXBuildFile', **values)

def phase(label, isa, files, **fields):
    return add(label, isa, buildActionMask=2147483647, files=files, runOnlyForDeploymentPostprocessing=0, **fields)

swift = {p: file(p) for p in ['App/H264ProxyApp.swift', 'Shared/Configuration.swift', 'Shared/Keychain.swift', 'PacketTunnel/PacketTunnelProvider.swift']}
metadata = [file(p, 'text.plist.xml') for p in ['App/Info.plist', 'App/App.entitlements', 'PacketTunnel/Info.plist', 'PacketTunnel/PacketTunnel.entitlements']]
metadata += [file('PacketTunnel/Bridge.h', 'sourcecode.c.h')]
base = file('Config/Base.xcconfig', 'text.xcconfig')
frameworks = {n: file('System/Library/Frameworks/' + n + '.framework', 'wrapper.framework', 'SDKROOT') for n in ['SwiftUI', 'Foundation', 'NetworkExtension', 'Network', 'Security', 'CoreFoundation', 'SystemConfiguration']}
archive = file('../../build/apple/libH264Engine.a', 'archive.ar')
app_product = add('app-product', 'PBXFileReference', explicitFileType='wrapper.application', path='H264Proxy.app', sourceTree='BUILT_PRODUCTS_DIR')
ext_product = add('extension-product', 'PBXFileReference', explicitFileType='wrapper.app-extension', path='H264PacketTunnel.appex', sourceTree='BUILT_PRODUCTS_DIR')
products = add('products', 'PBXGroup', children=[app_product, ext_product], name='Products', sourceTree='<group>')
framework_group = add('frameworks', 'PBXGroup', children=[*frameworks.values(), archive], name='Frameworks', sourceTree='<group>')
main_group = add('main-group', 'PBXGroup', children=[*swift.values(), *metadata, base, framework_group, products], sourceTree='<group>')
app_sources = phase('app-sources', 'PBXSourcesBuildPhase', [build('app:' + p, swift[p]) for p in ['App/H264ProxyApp.swift', 'Shared/Configuration.swift', 'Shared/Keychain.swift']])
ext_sources = phase('extension-sources', 'PBXSourcesBuildPhase', [build('extension:' + p, swift[p]) for p in ['PacketTunnel/PacketTunnelProvider.swift', 'Shared/Configuration.swift', 'Shared/Keychain.swift']])
app_frameworks = phase('app-frameworks', 'PBXFrameworksBuildPhase', [build('app-framework:' + n, frameworks[n]) for n in ['SwiftUI', 'NetworkExtension', 'Security']])
ext_frameworks = phase('extension-frameworks', 'PBXFrameworksBuildPhase', [build('extension-framework:' + n, frameworks[n]) for n in ['Foundation', 'NetworkExtension', 'Network', 'Security', 'CoreFoundation', 'SystemConfiguration']] + [build('engine-archive', archive)])
engine_script = add('engine-script', 'PBXShellScriptBuildPhase', buildActionMask=2147483647, files=[], inputPaths=[], outputPaths=[], alwaysOutOfDate=1, runOnlyForDeploymentPostprocessing=0, shellPath='/bin/bash', shellScript='set -euo pipefail\n"${SRCROOT}/../../scripts/build_ne.sh" --engine-only\n', name='Build embedded Go engine')
embed = phase('embed-extension', 'PBXCopyFilesBuildPhase', [build('embed-extension', ext_product, ATTRIBUTES=['CodeSignOnCopy', 'RemoveHeadersOnCopy'])], dstPath='', dstSubfolderSpec=13, name='Embed Packet Tunnel')
proxy = add('extension-proxy', 'PBXContainerItemProxy', containerPortal=ident('project'), proxyType=1, remoteGlobalIDString=ident('extension-target'), remoteInfo='H264PacketTunnel')
dependency = add('extension-dependency', 'PBXTargetDependency', target=ident('extension-target'), targetProxy=proxy)

def configurations(name, settings, project_settings=False):
    values = []
    for mode in ['Debug', 'Release']:
        config = dict(settings)
        config['SWIFT_OPTIMIZATION_LEVEL'] = '-Onone' if mode == 'Debug' else '-O'
        if mode == 'Debug':
            config['SWIFT_ACTIVE_COMPILATION_CONDITIONS'] = 'DEBUG'
        fields = {'buildSettings': config, 'name': mode}
        if project_settings:
            fields['baseConfigurationReference'] = base
        values.append(add(name + ':' + mode, 'XCBuildConfiguration', **fields))
    return add(name + ':list', 'XCConfigurationList', buildConfigurations=values, defaultConfigurationIsVisible=0, defaultConfigurationName='Release')

project_configs = configurations('project-config', {'SDKROOT': 'macosx', 'CLANG_ENABLE_MODULES': 'YES'}, True)
app_configs = configurations('app-config', {'PRODUCT_NAME': 'H264Proxy', 'PRODUCT_BUNDLE_IDENTIFIER': 'com.roca.h264proxy.mac', 'SWIFT_MODULE_NAME': 'H264ProxyApp', 'INFOPLIST_FILE': 'App/Info.plist', 'GENERATE_INFOPLIST_FILE': 'NO', 'CODE_SIGN_ENTITLEMENTS': 'App/App.entitlements', 'LD_RUNPATH_SEARCH_PATHS': ['$(inherited)', '@executable_path/../Frameworks']})
ext_configs = configurations('extension-config', {'PRODUCT_NAME': 'H264PacketTunnel', 'PRODUCT_BUNDLE_IDENTIFIER': 'com.roca.h264proxy.mac.PacketTunnel', 'SWIFT_MODULE_NAME': 'H264PacketTunnel', 'INFOPLIST_FILE': 'PacketTunnel/Info.plist', 'GENERATE_INFOPLIST_FILE': 'NO', 'CODE_SIGN_ENTITLEMENTS': 'PacketTunnel/PacketTunnel.entitlements', 'SWIFT_OBJC_BRIDGING_HEADER': 'PacketTunnel/Bridge.h', 'HEADER_SEARCH_PATHS': ['$(inherited)', '$(SRCROOT)/../../build/apple/include'], 'OTHER_LDFLAGS': ['$(inherited)', '-lresolv', '-e', '_NSExtensionMain'], 'SKIP_INSTALL': 'YES', 'APPLICATION_EXTENSION_API_ONLY': 'YES', 'LD_RUNPATH_SEARCH_PATHS': ['$(inherited)', '@executable_path/../Frameworks', '@executable_path/../../../../Frameworks']})
app = add('app-target', 'PBXNativeTarget', buildConfigurationList=app_configs, buildPhases=[app_sources, app_frameworks, embed], buildRules=[], dependencies=[dependency], name='H264Proxy', productName='H264Proxy', productReference=app_product, productType='com.apple.product-type.application')
ext = add('extension-target', 'PBXNativeTarget', buildConfigurationList=ext_configs, buildPhases=[engine_script, ext_sources, ext_frameworks], buildRules=[], dependencies=[], name='H264PacketTunnel', productName='H264PacketTunnel', productReference=ext_product, productType='com.apple.product-type.app-extension')
project_id = add('project', 'PBXProject', attributes={'LastUpgradeCheck': '1500', 'TargetAttributes': {app: {'CreatedOnToolsVersion': '15.0'}, ext: {'CreatedOnToolsVersion': '15.0'}}}, buildConfigurationList=project_configs, compatibilityVersion='Xcode 14.0', developmentRegion='zh_CN', hasScannedForEncodings=0, knownRegions=['zh_CN', 'en', 'Base'], mainGroup=main_group, productRefGroup=products, projectDirPath='', projectRoot='', targets=[app, ext])

def encode(value, level=0):
    indent = '\t' * level
    if isinstance(value, dict):
        return '{\n' + ''.join('\t' * (level + 1) + json.dumps(str(k)) + ' = ' + encode(v, level + 1) + ';\n' for k, v in value.items()) + indent + '}'
    if isinstance(value, list):
        return '(' + ', '.join(encode(v, level) for v in value) + (',' if value else '') + ')'
    if isinstance(value, int):
        return str(value)
    return json.dumps(value)

project.mkdir(parents=True, exist_ok=True)
(project / 'project.pbxproj').write_text('// !$*UTF8*$!\n' + encode({'archiveVersion': 1, 'classes': {}, 'objectVersion': 56, 'objects': objects, 'rootObject': project_id}) + '\n')
schemes = project / 'xcshareddata/xcschemes'
schemes.mkdir(parents=True, exist_ok=True)
reference = f'<BuildableReference BuildableIdentifier="primary" BlueprintIdentifier="{app}" BuildableName="H264Proxy.app" BlueprintName="H264Proxy" ReferencedContainer="container:H264Proxy.xcodeproj"/>'
(schemes / 'H264Proxy.xcscheme').write_text(f'''<?xml version="1.0" encoding="UTF-8"?>
<Scheme LastUpgradeVersion="1500" version="1.3">
<BuildAction parallelizeBuildables="YES" buildImplicitDependencies="YES"><BuildActionEntries><BuildActionEntry buildForTesting="YES" buildForRunning="YES" buildForProfiling="YES" buildForArchiving="YES" buildForAnalyzing="YES">{reference}</BuildActionEntry></BuildActionEntries></BuildAction>
<TestAction buildConfiguration="Debug"/>
<LaunchAction buildConfiguration="Debug" selectedDebuggerIdentifier="Xcode.DebuggerFoundation.Debugger.LLDB" selectedLauncherIdentifier="Xcode.IDEFoundation.Launcher.LLDB" launchStyle="0" useCustomWorkingDirectory="NO" ignoresPersistentStateOnLaunch="NO" debugDocumentVersioning="YES" debugServiceExtension="internal" allowLocationSimulation="YES"><BuildableProductRunnable runnableDebuggingMode="0">{reference}</BuildableProductRunnable></LaunchAction>
<ProfileAction buildConfiguration="Release"><BuildableProductRunnable runnableDebuggingMode="0">{reference}</BuildableProductRunnable></ProfileAction>
<AnalyzeAction buildConfiguration="Debug"/>
<ArchiveAction buildConfiguration="Release" revealArchiveInOrganizer="YES"/>
</Scheme>
''')
print(project)
