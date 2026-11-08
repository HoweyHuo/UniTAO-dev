/*
************************************************************************************************************
Copyright (c) 2022 Salesforce, Inc.
All rights reserved.

UniTAO was originally created in 2022 by Shai Herzog & Yi Huo as an
Universal No-Coding Heterogeneous Infrastructure Maintenance & Inventory system that is holistically driven by open/community-developed semantic models/schemas.

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as published
by the Free Software Foundation, either version 3 of the License, or
(at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program.  If not, see <https://www.gnu.org/licenses/>

This copyright notice and license applies to all files in this directory or sub-directories, except when stated otherwise explicitly.
************************************************************************************************************
*/

package main

import (
	"crypto/ed25519"
	"flag"
	"fmt"
	"log"
	"os"

	"DataService/Channel"
)

const (
	KEYGEN      = "keygen"
	FINGERPRINT = "fingerprint"
	ENCKEY      = "enckey"
)

// RunKeyCommand 处理不需要 config 的密钥子命令；不是密钥子命令时返回 false。
//
//	keygen      -out <file>                         生成 shared vDS 的签名私钥（ed25519 PKCS8 PEM），输出指纹
//	fingerprint -key <file>                         输出签名私钥对应的指纹（填到镜像方的 trustedFingerprint）
//	enckey      -identity <file> -recipient <file>  生成加密密钥对：identity 给镜像方（decrypt.identityFile），
//	                                                recipient 一行追加到共享方的 encrypt.recipientsFile
func RunKeyCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	switch args[1] {
	case KEYGEN:
		cmd := flag.NewFlagSet(KEYGEN, flag.ExitOnError)
		out := cmd.String("out", "", "path of the signing key file to create")
		cmd.Parse(args[2:])
		if *out == "" {
			cmd.Usage()
			log.Fatal("missing -out")
		}
		pemData, pub, err := Channel.GenerateSigningKey()
		if err != nil {
			log.Fatalf("failed to generate signing key, Err:%s", err)
		}
		writeNew(*out, pemData, 0o600)
		fmt.Printf("signing key written to %s\nfingerprint: %s\n", *out, Channel.Fingerprint(pub))
	case FINGERPRINT:
		cmd := flag.NewFlagSet(FINGERPRINT, flag.ExitOnError)
		key := cmd.String("key", "", "path of the signing key file")
		cmd.Parse(args[2:])
		if *key == "" {
			cmd.Usage()
			log.Fatal("missing -key")
		}
		priv, err := Channel.LoadSigningKey(*key)
		if err != nil {
			log.Fatal(err)
		}
		fmt.Println(Channel.Fingerprint(priv.Public().(ed25519.PublicKey)))
	case ENCKEY:
		cmd := flag.NewFlagSet(ENCKEY, flag.ExitOnError)
		identityFile := cmd.String("identity", "", "path of the private identity file to create (for the mirrored side)")
		recipientFile := cmd.String("recipient", "", "path of the public recipient file to create (for the shared side)")
		cmd.Parse(args[2:])
		if *identityFile == "" || *recipientFile == "" {
			cmd.Usage()
			log.Fatal("missing -identity or -recipient")
		}
		identity, recipient, err := Channel.GenerateEncryptionKey()
		if err != nil {
			log.Fatalf("failed to generate encryption key, Err:%s", err)
		}
		writeNew(*identityFile, []byte(identity+"\n"), 0o600)
		writeNew(*recipientFile, []byte(recipient+"\n"), 0o644)
		subscriberId, err := Channel.RecipientSubscriberId(recipient)
		if err != nil {
			log.Fatalf("failed to derive subscriber id, Err:%s", err)
		}
		fmt.Printf("identity (keep private, for decrypt.identityFile): %s\nrecipient (append to encrypt.recipientsFile): %s\nsubscriber id: %s\n", *identityFile, *recipientFile, subscriberId)
	default:
		return false
	}
	return true
}

// writeNew 写入新文件，已存在时拒绝覆盖（避免误删密钥）。
func writeNew(path string, data []byte, perm os.FileMode) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		log.Fatalf("failed to create [%s], Err:%s", path, err)
	}
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		log.Fatalf("failed to write [%s], Err:%s", path, err)
	}
}
