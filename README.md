# webBenchmark
http benchmark tool to ran out your server bandwidth.

用户在使用本工具前请先查看授权及免责声明，webBenchmark仅仅是一个用于测试网页服务器性能的工具，用作其他用途，后果自负。

- random User-Agent on every Request
- customizable Referer Url,
- concurrent routines as you wish, depends on you server performance.
- add http post mode
- specify target ip, or resolved by system dns.
- randomly X-Forwarded-For and X-Real-IP (default on).

# Todo 
- automatically tune concurrent routines to gain maximum performance. 
- randomly target ip.
- support NOT standard port in address with specify target ip.

# Usage
    webBenchmark -c [COUNT] -s [URL] -r [REFERER]
    -c int
          concurrent routines for download (default 16)
    -r string
          referer url
    -s string
          target url (default "https://baidu.com")
    -i string
          customize ip
    -f string
          randomized X-Forwarded-For and X-Real-IP address
    -p string
          post content

# Linux
    wget https://github.com/maintell/webBenchmark/releases/download/0.3/webBenchmark_linux_x64
    chmod +x webBenchmark_linux_x64
    ./webBenchmark_linux_x64 -c 32 -s https://target.url

## Advanced example
    # send request to 1.1.1.1 for https://target.url with 32 concurrent threads 
    # and refer is https://refer.url
    ./webBenchmark_linux_x64 -c 32 -s https://target.url -r https://refer.url -i 1.1.1.1

##LICENSE AND DISCLAIMER

**1. Application.**

Please read this document carefully before using, accessing, downloading, installing or otherwise operating the webBenchmark as defined hereafter.

Using, accessing, downloading or otherwise operating any of the webBenchmark, constitutes an unconditional agreement by You to be bound by this the following terms and conditions for the time of Using the webBenchmark and thereafter.

IF YOU DO NOT ACCEPT THE TERMS OF THIS LICENSE AGREEMENT, YOU ARE PROHIBITED FROM USING ANY OF THE webBenchmark.

**2. Definitions.**

**"webBenchmark"** shall mean any of the documents, description, explanations, presentations, media types, all schedules, appendixes and related documentation, software in object or source code, including Updates provided on this Platform by Licensor for Your Use.

**"Derivative Works"** means any modification, change, adaptations, contributions, enhancements, customization, modifications, inventions, developments, improvements of the Date Product by you and not developed by Licensor or integrated into the Date Product by Licensor.

**"Intellectual Property Rights"** means any intellectual property and proprietary rights, including , but not limited to, copyrights, moral rights, works of authorship, trade and service marks, trade names, rights in logos and get-up, inventions and discoveries, and Know-How, registered designs, design rights, patents, utility models, all rights of whatsoever nature in computer software and data, source code, database rights all intangible rights and privileges of nature similar or allied to any of the foregoing, in every case in any part of the world and whether or not registered; and including all granted registrations and all applications for registration, all renewals, reversions or extensions, the right to sue for damages for past infringement and all forms of protection of a similar nature which may subsist anywhere in the world.

**"Know-How"** means any information relating to commercial, scientific and technical matters, inventions and trade secrets, including but not limited to any patentable technical or other information which is not in the public domain including information comprising or relating to concepts, discoveries, data, designs, formulae, ideas, reports and data analyses.

**"License"** shall mean this license and disclaimer document and its terms and conditions for use, reproduction, and distribution as provided in this document.

**"Licensor"** shall mean the copyright owner or entity authorized by the copyright owner that is granting the License, meaning maintell, and its successors and assigns.

**"Parties"** means both You and Licensor.

**"Party"** means You or Licensor individually.

**"Platform"** means the maintell GitHub account and related repositories available at https://github.com/maintell.

**"Purpose"** means using or integrating the webBenchmark free of charge for the purpose of using and integrating benchmarking on a website, whereby examples are provided in the webBenchmark to demonstrate specific features.

**"SDK"** means a software development kit which is a set of software development tools that allows the creation of applications for a certain software package, video service platforms, software framework, or similar development platform.

**"maintell"** means a set of tools written and developed by the Licensor that provides support for benchmark and related functionalities for HTTP including any related software, source and object code, deliverables, technology and related resources and relevant documentation provided and/or created, made available, license and/or sold to you and developed by Licensor in connection with separate license terms and conditions.

**"Use"** means using, accessing, downloading, installing or otherwise operating or using the webBenchmark as part of Your self-service and subject to clause titled "LICENSE" and in connection with the Purpose of this License and its terms and conditions.

**"Updates"** means all updates, modifications and releases of new versions of webBenchmark containing improvements, corrections, minor modifications, bug fixes, patches, or the like that have been added to the Platform by the Licensor.

"**You" (or "Your")** shall mean an individual or legal entity exercising permissions granted by this License.

**3. License**

Subject to the terms and conditions of this License, Licensor hereby grants to You a perpetual, worldwide, non-exclusive, no-charge, royalty-free, right to reproduce, prepare Derivative Works of, sublicence, make, have made, use, import the webBenchmark and the Derivative Works as required for the Purpose and subject to the terms and conditions as described in the Date Products.

Except as otherwise agreed by Licensor in writing in separate license terms and conditions for the use of maintell, You shall not distribute, relicense, sell, lease, transfer, encumber, assign or make available for public use the webBenchmark. Any attempt to take any such actions is void and will automatically terminate Your rights under this License.

If the webBenchmark or your Use (allegedly) constitutes a direct or contributory infringement, then any

rights granted to You under this License for that webBenchmark shall terminate immediately.

Unless agreed by Parties in writing or if the enforcement of this provision is prohibited by applicable law, You shall not under any circumstances attempt, or knowingly cause or permit others to attempt to modify, adapt, port, merge, decompile, disassemble, reverse engineer, decipher, decrypt or otherwise discover the source code or any other parts of the mechanisms and algorithms used by webBenchmark nor remove restrictions or create derivative works of webBenchmark or of any part of webBenchmark.

**4. Support**

The Licensor has no obligation under this License to provide any maintenance, support or training to You.

**5. Update**

The Licensor may at any time, at its discretion provide Updates to the webBenchmark. The Licensor has, however, no obligation whatsoever under this License to provide Updates, modify or release new versions of the webBenchmark.

**6. Submission of Contributions**

Any contribution submitted for inclusion in the webBenchmark by You to the Licensor shall be under the terms and conditions of this License, without any additional terms or conditions. Such inclusion shall be subject to Licensor&#39;s discretion. Notwithstanding the above, nothing herein shall supersede or modify the terms of any separate license agreement you may have executed with Licensor regarding such contributions.

**7. Trademarks.**

This License does not grant permission to use the trade names, trademarks, service marks, or product names of the Licensor, except as required for reasonable and customary use in describing the origin of the webBenchmark and related copyright notices.

**8. Intellectual Property**

You recognize that all rights, title and interests in and to any and all worldwide Intellectual Property Rights related to the webBenchmark shall remain the property of Licensor or its suppliers. Unless otherwise agreed upon between the Parties, any Intellectual Property Rights in any Updates, contributions, enhancements, customization, modifications, inventions, developments, improvements thereof of any kind to, in, or that otherwise relate to the webBenchmark, including any Derivative Work or results of agreed services during, before or after the term of this License, either specific to You, Your customer or in general in connection with this License or arising out of the business relationship between the Parties shall solely and exclusively belong to or be transferred to Licensor through assignment, entitlement or otherwise, including the entire right, title and interest. For this purpose, Licensor shall also have the right to file and prosecute at its own expenses any patent application on the same above, in any country, region or jurisdiction in the world in its own name or on behalf of You, as the case may be. You shall not have the right to claim and will not undertake or try to obtain, register or apply for any Intellectual Property Rights or other rights in or to the webBenchmark or Derivative Works anywhere in the world. You shall not do anything that might misrepresent, change or otherwise compromises the ownership or proprietary rights of Licensor or its suppliers under this License. You shall not take any actions that would amount to an exhaustion of Licensor&#39;s or its suppliers Intellectual Property Rights. The webBenchmark may contain the logo and copyright notice of Licensor. It is prohibited to remove or modify the copyright notice and webBenchmark logo of Licensor.

**9. Disclaimer of Warranty.**

Unless required by applicable law or agreed by the Parties in writing, Licensor provides the webBenchmark AND any RELATEd SERVICE on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied, STATUTORY OR OTHERWISE, including, without limitation, any warranties or conditions of THE webBenchmark ACCURACY, TITLE, MERCHANTABILITY, or FITNESS FOR A PARTICULAR PURPOSE OR NON-INFRINGEMENT. THE LICENSOR DOES NOT PROVIDE ANY WARRANTY AS TO QUALITY, SUITABILITY, FEATURES, COMPATIBILITY OF THE webBenchmark AND RELATED SERVICES. THIS AGREEMENT DOES NOT PROVIDE ANY REPRESENTATION OR WARRANTY OR LIABILITY AS TO ANY THIRD-PARTY SOFTWARE.

**10. Third Party Software Disclaimer**

The webBenchmark may make reference to third party standard software (e.g. open source software and video test streams) which is not developed by Licensor, but which are provided in connection with the Purpose of the integration or testing of the maintell. For the avoidance of doubt, Licensor is not a sub licensor of such third party software. Licensor refers Licensee to applicable attribution files and license terms disclosures and pertinent terms of the respective third-party standard software publisher which apply directly to Licensee. However, Parties will ensure their compliance with such relevant licensing terms.

THIS THIRD-PARTY SOFTWARE IS PROVIDED BY THE RESPECTIVE COPYRIGHT HOLDERS AND CONTRIBUTORS "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT HOLDER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL, SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE, DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE OF THIS THIRD PARTY SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

**11. Limitation of Liability.**

You are solely responsible or liable for determining the appropriateness of Using the webBenchmark AND RELATED SERVICES and assume any risks associated with Your exercise of permissions under this License and the creation of Derivative Works. Licensor shall have no liability of any kind with regards TO such Derivative Works.

In no event and under no legal theory, whether in tort (including negligence), contract, or otherwise, unless required by applicable law (such as deliberate and grossly negligent acts) or agreed to in writing, shall Licensor be liable to You for damages OF ANY KIND, including any direct, indirect, special, incidental, or consequential damages of any character arising as a result of this License or out of the use or inability to use the webBenchmark or Derivative Works, including but not limited to damages for loss of goodwill, LOST REVENUE, LOST PROFIT, LOST DATA OR CORRUPTED DATA, COSTS OF PROCUREMENT FOR SUBSTITUTION OF PRODUCTS OR SERVICES, THIRD PARTY SOFTWARE AND CLAIMS, PROVIDED INFORMATION, WASTED MANAGEMENT TIME, LOSS OF USE OF COMPUTER SYSTEMS AND RELATED EQUIPMENT, COMPUTER FAILURE AND MALFUNCTIONS, DOWNTIME COST, work stoppage, or any and all other commercial damages or losses, even if such the Licensor or the Contributor has been advised of the possibility of such damages.

THE PROVISIONS OF THIS CLAUSE TITLED "LIMITATION OF LIABILITY" SHALL NOT APPLY TO THE EXTENT RESTRICTED OR PREVENTED BY MANDATORY APPLICABLE LAW THAT CANNOT BE AMENDED OR EXCLUDED BY CONTRACTUAL WAIVER SUCH AS DELIBERATE ACTS AND FRAUD.

**12. Derivative Work**

While creating and using Derivative Works, if you choose to offer additional warranty, indemnity, or other liability obligations and/or rights inconsistent with this License, You act only on Your own behalf and on Your sole responsibility, not on behalf of the Licensor. You agree to indemnify, defend, and hold the Licensor harmless for any liability incurred by, or claims asserted against, the Licensor by reason of your accepting any such warranty or additional obligations and liability.

**13. Third Parties**

The Licensor will not indemnify nor hold harmless You against any infringements of any rights of third parties with respect to the webBenchmark or the Derivative Works.

Licensor shall have no obligation for payment of royalties or any other compensation to You or third parties, if any, with respect to the Use of the webBenchmark by You or Your customers, clients, viewers, listeners for playing media content or in connection with third party products and software. You will be exclusively responsible for payment of royalties to third parties.

**14. Legal Capacity**

By accepting this License, You represent and warrant to have the legal capacity and authority to enter into this legally binding License.

**15. No Implied Rights**

Other than expressly provided for in this License, nothing in this License grants or shall be construed to grant to any Party any right and/or any license to any Intellectual Property Right or application therefore (including but not limited to patent applications or patents) which are held by and/or in the name of the other Party and/or which are controlled by the other Party, or to any Confidential Information received from the other Party.

**16. Indemnification**

You agree, at Licensor&#39;s option, to release, defend, indemnify, and hold Licensor and its affiliates and subsidiaries, and their officers, directors, employees, contractors and agents, harmless from and against any claims, liabilities, damages, losses, and expenses, including, without limitation, reasonable legal and accounting fees, arising out of or in any way connected with (i) Your breach of this License (ii) Your negligent or improper use, misuse or intentional omission in connection with the use of the webBenchmark or any of Licensor&#39;s services.

**17. Notices**

All notices or other communication required or permitted to be given in writing under this License must be given in the English and Chinese language by email.

**18. Waivers**

No failure or delay by any Party in exercising any right or remedy provided by law or pursuant to this License will impair such right or remedy or be construed as a waiver of it and will not preclude its exercise at any subsequent time and no single or partial exercise of any such right or remedy will preclude any further exercise of it or the exercise of any other remedy.

**19. Severability**

If any provision of this License or of any of the documents contemplated in it is held to be invalid or unenforceable, then such provision will (so far as it is invalid or unenforceable) have no effect and will be deemed not to be included in this License or the relevant document, but without invalidating any of the remaining provisions of this License or that document. The Parties must then use all reasonable endeavors to replace the invalid or unenforceable provision by a valid and enforceable substitute provision the effect of which is as close as possible to the intended effect of the invalid or unenforceable provision.

**20. Modifications**

The Licensor may modify the terms of this License in its sole discretion and such modifications shall take effect and be binding on You on the earliest date which they are posted to the Platform. No one other than the Licensor has the right to modify this License.

**21. GOVERNING LAW AND JURISDICTION**

The License is governed by and must be construed, interpreted in accordance with the laws of CHINA without given effect to the conflict of law principles thereof. The courts of CHINA have exclusive jurisdiction over any dispute, legal action and proceedings arising out of or related to the License, including its termination, which shall be binding and enforceable upon the Parties worldwide. In the event of any proceeding or litigation arising out of this License, the prevailing Party shall be entitled to recover from the non-prevailing Party its legal fees, court fees and related costs to the extent and in ratio of its success. Notwithstanding the foregoing, Licensor may bring legal actions against You in the country where You has its seat, if it deems necessary for the enforceability of its rights regarding payments by You under the License.

