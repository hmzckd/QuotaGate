# QuotaGate

QuotaGate, çok müşterili bir Go uygulamasının **isteği çalıştırmadan önce** müşteri ve işlem için izin kararı almasını sağlayan dar kapsamlı bir servis çalışmasıdır. Hedef, Go ile API sınırları, eşzamanlı kota güncellemesi, tekrar deneme ve bağımlılık arızası davranışını gösteren yerel bir portföy demosudur. QG-01–04 kalıcı günlük kota ve middleware kullanan örnek API'yi, QG-05 ayrı Redis hız ön kontrolünü, QG-06 birleşik hız/günlük kararını, QG-07 sınırlı istemci tekrarını, QG-08 iki kopyalı gerçek bağımlılık kesintisi ve toparlanmayı içerir. **v0.1 ve v0.2 kabulü tamamlandı**; gözlemlenebilirlik ve performans ölçümü v0.3 görevlerindedir.

## İstek akışı

1. Müşteri, kendi QuotaGate anahtarıyla örnek API'de `POST /jobs` çağırır.
2. Örnek API'deki middleware her **gelen istek** için yeni bir karar kimliği üretir ve `operation=job.create` için `POST /v1/decisions` çağırır. Geçici bağlantı/yanıt hatasında aynı kimlikle, toplam iki saniye içinde en fazla üç deneme yapar.
3. QuotaGate anahtardan müşteriyi belirler. `serve-combined` modunda önce PostgreSQL'deki kayıtlı kararı arar; yeni kararda Redis hız kontrolünü, ardından PostgreSQL günlük kotasını değerlendirip nihai sonucu kaydeder.
4. İzin verilirse örnek API handler'ı çalışır. Ret gelirse `429`, karar alınamazsa `503` döner ve handler çalışmaz. `GET /v1/usage` yalnızca anahtarın sahibinin kalıcı günlük kullanımını gösterir.

Günlük kullanım **başarıyla tamamlanan iş** değil, QuotaGate'in kabul edip hak ayırdığı karardır. Handler hata verse veya istemciye yanıt ulaşmasa da ayrılan hak geri verilmez. İstemcinin örnek API isteğini yeniden göndermesi yeni bir karar ve yeni bir kullanım olabilir. Bu servis dış yan etki için exactly-once güvencesi vermez.

Servis trafik proxy'si değildir: genel routing, load balancing, TLS sertifika yönetimi, WAF, OAuth/SSO, eklenti, ödeme/faturalama, Kafka/NATS ve genel API Gateway özellikleri kapsam dışıdır. Yerel örnek API yalnızca kararın nasıl kullanıldığını gösterir. AWS, bu planın bitiş koşulu değildir.

Sürüm ve görevler: [PLAN.md](PLAN.md). Devam noktası: [STATUS.md](STATUS.md).

## QG-01 yerel çalıştırma

Gerekenler: Go 1.27+, Docker Desktop/Compose; duman testi için PowerShell 7. Mevcut kod sağlık, müşteri/politika, karar ve kullanım uçları ile örnek API'yi içerir.

```powershell
Copy-Item .env.compose.example .env.compose
# .env.compose içindeki QG_DB_PASSWORD ve QG_ADMIN_TOKEN değerlerini yalnızca bu yerel kurulum için değiştirin.
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
Invoke-WebRequest http://127.0.0.1:18080/health/live
Invoke-WebRequest http://127.0.0.1:18080/health/ready
Invoke-WebRequest http://127.0.0.1:18081/health/live
docker compose -p quotagate-qg01 --env-file .env.compose run --rm migrate
docker compose -p quotagate-qg01 --env-file .env.compose down
```

`/health/live` HTTP sürecinin çalıştığını, `/health/ready` PostgreSQL'e 1 saniye içinde erişilebildiğini gösterir. Migration dosyaları `migrations/` altındadır; uygulanan dosyanın checksum'ı kaydedilir ve sonraki çalıştırmada değişmiş dosya reddedilir. `down` hacmi silmez; yerel test verisinin temizliği ayrı ve açık bir karardır. `.env.compose` Git dışında tutulur. Compose gerçek çalıştırmasının durumu [STATUS.md](STATUS.md) içinde kayıtlıdır.

## QG-02 yönetim ve müşteri uçları

Bu sürümde yönetici ve müşteri uçları `Authorization: Bearer <token>` kullanır. `ADMIN_TOKEN` yalnızca yönetim uçlarında geçerlidir. Yeni müşteri anahtarı `qg_` önekli 256 bit rastgele değerdir; yalnızca oluşturma veya yenileme yanıtında görünür, veritabanında SHA-256 hash'i bulunur. Anahtarlar ve yönetici sırrı uygulama loglarına yazılmaz. Bu yerel HTTP demosu için düz HTTP yalnızca `127.0.0.1` üzerinden yayımlanır.

| İşlem | Uç | Sonuç |
| --- | --- | --- |
| Müşteri oluştur | `POST /v1/admin/customers` `{ "name": "A" }` | `201`, müşteri kimliği ve tek seferlik anahtar |
| Politika yaz | `PUT /v1/admin/customers/{id}/policies/{operation}` `{ "daily_limit": 100, "rate_limit_per_minute": 10 }` | `200`, sürümlü politika |
| Anahtar yenile | `POST /v1/admin/customers/{id}/rotate-key` | `200`, yeni tek seferlik anahtar; eski anahtar `401` |
| Müşteriyi kapat | `POST /v1/admin/customers/{id}/disable` | `204`, mevcut anahtar `401` |
| Kendi kimliğini oku | `GET /v1/me` | `200`, anahtardan bulunan müşteri |
| Kendi politikasını oku | `GET /v1/policies/{operation}` | `200`, anahtardan bulunan müşterinin politikası |

Yönetim yolları dışındaki müşteri uçları `customer_id` almaz. Politika adı örneği `job.create` biçimindedir. QG-02 yalnızca limitleri kaydediyordu; QG-03 ile günlük karar ve kullanım eklendi.

## QG-03 günlük karar ve kullanım

Müşteri anahtarıyla `POST /v1/decisions` gövdesi `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` gönderilir. Yanıt `200` ve `allowed`, `reason`, `utc_day`, `daily_used`, `daily_limit`, `policy_version` alanlarını taşır. `allowed=false, reason=daily_quota` karar servisi yanıtıdır; örnek API middleware'i bunu `429` olarak kullanır. Tanımsız işlem `403`, aynı karar kimliğinin farklı işlemle kullanılması `409`, geçersiz anahtar `401`, depolama kesintisi `503` verir.

`GET /v1/usage?operation=job.create`, anahtarın sahibinin **bugünkü UTC** kullanımını, limitini ve `resets_at_utc` değerini döndürür. `customer_id` parametresi kabul edilmez. Gün, PostgreSQL'in `clock_timestamp()` zamanından seçilir. Aynı müşteri ve karar kimliğiyle tekrar, ilk kaydedilen sonucu politika veya gün değişse de döndürür; yeni karar kimliği yeni bir hak talebidir.

Günlük hak, izin kararı transaction'da kaydedilince ayrılır. Sonraki handler başarısız olsa bile geri verilmez. Müşteri satırı kilidi tekrarları sıralar; koşullu PostgreSQL artışı ve karar kaydı aynı transaction'dadır. Temel `serve` modunda `/v1/decisions` günlük kota uygular; `serve-combined` modunda önce Redis hız kontrolü de uygulanır. Harici yan etkinin exactly-once çalışması vaat edilmez.

## QG-04 middleware ve örnek API

`client.New("http://127.0.0.1:18080")` ile oluşturulan Go istemcisi birden fazla müşterinin eşzamanlı isteklerinde paylaşılabilir. `decisions.Middleware("job.create", handler)` müşteri anahtarını gelen `Authorization: Bearer ...` header'ından alır; gelen gövdeyi tüketmeden QuotaGate kararı ister. İstemci aynı karar kimliği/gövde/anahtarla en fazla üç deneme yapar; toplam iki saniyelik bütçe ve gelen isteğin iptali tüm denemeleri ve beklemeleri kapsar. Yönlendirmeler izlenmez; eksik, bozuk veya çelişkili karar yanıtı handler'ı çalıştırmaz. Uygulama kapanırken `decisions.Close()` boş bağlantıları kapatır.

Compose örnek API'yi `127.0.0.1:18081` üzerinde açar. `POST /jobs` izin halinde `201` ve süreç içinde numaralanmış `demo-N` iş kimliği verir. Kota reddi `429`, karar alınamaması `503`, geçersiz anahtar `401`, tanımsız işlem politikası `403` olur. Her izin handler'ı bir kez çalıştırır; diğer sonuçlarda handler çalışmaz. Gelen `Idempotency-Key` veya gövdedeki karar kimliği tekrar kullanımı sağlamaz: ayrı API isteği yeni karar ve hak talebidir.

`GET /demo/stats` tüm müşteriler için toplam `handler_calls` sayısını gösteren yerel kanıttır; örnek API yeniden başladığında sıfırlanır. Kalıcı müşteri kullanımı QuotaGate'in `GET /v1/usage?operation=job.create` ucundan alınır. Örnek iş gerçek bir işe veya harici API'ye bağlı değildir. Örnek `/health/live` yalnızca örnek HTTP sürecini gösterir; QuotaGate'in bağımlılık durumu kendi `/health/ready` ucundadır.

İki müşteri, ret, kesinti ve toparlanma demosu:

```powershell
docker compose -p quotagate-qg01 --env-file .env.compose up --build -d --wait
pwsh -File ./scripts/smoke-qg04.ps1
docker compose -p quotagate-qg01 --env-file .env.compose down
```

Betik iki geçici müşteri için günlük limit `2/3` yazar, anahtarları bellekte tutar ve aynı inbound header ile ayrı karar kimliklerini doğrular. QuotaGate `api` servisini kısa süre durdurup yeniden başlatır; ret ve kesintide handler sayacının değişmediğini kontrol eder. Sonuçları PostgreSQL sayaç/karar kayıtlarıyla uzlaştırır, uygulama loglarında sır arar ve yalnızca oluşturduğu iki müşterinin verisini temizler. Çalışan izole Compose ortamında başka trafik olmadan çalıştırın. Betik `.env.compose` içindeki portları okur; örnek API portu `QG_EXAMPLE_PORT` ile değiştirilebilir.

`go test ./... -count=1` middleware HTTP testlerini çalıştırır. Gerçek PostgreSQL testleri için ayrıca izole, migration uygulanmış veritabanını `QG_TEST_DATABASE_URL` ile verin; değişken yokken bu testler atlanır. Kabul koşularının sonuçları [STATUS.md](STATUS.md) içindedir.

## QG-05 ortak Redis ön kontrolü

`serve-rate-demo` modu, müşteri anahtarıyla `POST /demo/rate-checks` `{ "decision_id": "rastgele-en-az-16-karakter", "operation": "job.create" }` ucunu açar. Müşteri anahtardan, hız limiti PostgreSQL politikasından alınır. `200` yanıtı `allowed`, `reason=rate_allowed|rate_limited`, `rate_used`, `rate_limit`, `window_start_utc`, `resets_at_utc` alanlarını taşır. Bu bir **hız ön sonucu**dur; handler izni vermez, PostgreSQL günlük sayacını veya nihai karar tablosunu değiştirmez. Bu tarihsel demo modunda `/v1/decisions` günlük kota akışını kullanır; birleşik akış aşağıdaki `serve-combined` modundadır.

Lua script'i müşteri + işlem sayacını, Redis `TIME` değerinin düştüğü 60 saniyelik sabit pencereye göre artırır. Ret sayaç artırmaz. Sayaç anahtarının mutlak silinme zamanı pencere sonudur; müşteri + karar kimliğinin ön sonucu 24 saat tutulur. Aynı kimlik/işlem ilk izin veya ret sonucunu, eski limitini ve penceresini döndürür; tekrar saklama süresini uzatmaz. Başka işlemle aynı kimlik `409` olur. Anahtarlar müşteri/işlem/karar kimliklerinin hash'lerini kullanır; API anahtarı Redis'e verilmez. Script cache silinince `EVALSHA` → `EVAL` dönüşü vardır. [Redis Lua yürütmesi](https://redis.io/docs/latest/develop/programmability/eval-intro/), [TIME](https://redis.io/docs/latest/commands/time/) ve [PEXPIREAT](https://redis.io/docs/latest/commands/pexpireat/).

İki kopyalı yerel kabul ortamı:

```powershell
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml up --build -d --wait
pwsh -File ./scripts/smoke-qg05.ps1
docker compose -p quotagate-qg05 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml down
```

QG-05 ayrı `quotagate-qg05_pgdata` hacmini kullanır. Varsayılan portlar birinci API `18080`, ikinci API `18082`, Redis `56379`; Redis yalnızca loopback üzerinden yayımlanır ve `QG_REDIS_PORT` ile değiştirilebilir. QG-01–04 ortamıyla aynı HTTP/DB portlarını kullandığından iki ortamı sırayla çalıştırın. Redis bu demoda parolasız, diske yazmadan ve `128mb/noeviction` sınırıyla çalışır. Redis'in durması veya veri kaybı ön sonuçları kaybettirir; kalıcı günlük kullanım PostgreSQL'de kalır. Sabit pencere sınırında iki pencerenin kapasitesi kısa bir aralıkta kullanılabilir.

Betik iki kopyaya 60 benzersiz istek gönderir ve limit 7 altında 7 ön izin/53 ret bekler; 20 eşzamanlı aynı kimlikli isteğin tek ön hak tüketmesini, müşteri/işlem ayrımını, politika değişimi sonrası ilk sonucun korunmasını ve TTL'leri doğrular. PostgreSQL günlük kullanım/karar kayıtlarının oluşmadığını ve loglarda sır bulunmadığını kontrol eder; yalnızca kendi test müşterilerini ve Redis anahtarlarını siler.

Gerçek Redis testleri için `QG_TEST_REDIS_URL=redis://127.0.0.1:56379/15` verin ve `go test ./... -count=1` çalıştırın. Testler rastgele müşteri alanlarını temizler ve script cache dönüşünü doğrulamak için **izole test Redis'inde** `SCRIPT FLUSH` kullanır. Pencere sınırı testi, üretim Lua script'inin yalnızca saat ifadesini kontrollü değerle değiştirir; ayrı test gerçek Redis `TIME` ve mutlak key expiry'yi doğrular.

## QG-06 birleşik ve kalıcı karar

`serve-combined`, aynı `POST /v1/decisions` sözleşmesinde PostgreSQL kayıtlı sonuç → Redis ön kontrol → PostgreSQL günlük kota/nihai sonuç sırasını uygular. Yanıt `200` ve `{allowed, reason, utc_day, daily_used, daily_limit, policy_version}` biçimindedir. `reason`, `allowed`, `daily_quota` ve `rate_limited` değerlerini ayırır. Redis retleri de kalıcı karar olur; günlük sayaç oluşturmaz veya artırmaz. Müşteri kimliği anahtardan alınır; yabancı `customer_id` gövdesi ve query parametreleri `400` olur.

PostgreSQL transaction'ı kayıtlı karar aranmadan önce açılır. Müşteri satırı ve güncel politika kilitleri, bağlamla sınırlı Redis çağrısı ve kalıcılaştırma boyunca tutulur. Aynı müşteri kararları sıralanır; kayıtlı sonuç bulunduğunda Redis çağrılmaz. Gün, Redis kontrolünden sonra PostgreSQL `clock_timestamp()` değerinden seçilir; günlük sayaç ve nihai kayıt birlikte commit edilir. Bu yaklaşım müşteri başına eşzamanlılığı sınırlar ve Redis gecikmesi boyunca PostgreSQL bağlantısı tutar; performans ölçümü QG-11'dedir.

| Hata veya sonuç | Redis kapasitesi | PostgreSQL günlük kullanım ve karar |
| --- | --- | --- |
| Kimlik/politika/ilk DB okuması başarısız | Redis çağrılmaz | Yeni hak veya nihai kayıt yok |
| Redis çağrısı başarısız veya yanıtı kayıp | Lua tamamlandıysa kapasite tüketilmiş olabilir | Transaction geri alınır; `503` |
| Redis hız reddi | Yeni kapasite tüketmez | Günlük artmaz; `rate_limited` kaydedilir |
| Redis izin verdi, günlük kota dolu | Tüketilen kapasite korunur | Günlük artmaz; `daily_quota` kaydedilir |
| Redis izin verdi, PostgreSQL yazımı başarısız | Tüketilen kapasite korunur | Günlük artış ve kayıt geri alınır; `503` |
| PostgreSQL commit yanıtı belirsiz veya HTTP yanıtı kayıp | Tüketilen kapasite korunur | Commit olmuş olabilir; aynı kimlikle tekrar kayıtlı sonucu bulur |
| Nihai kararın aynı müşteri/işlem/kimlikle tekrarı | Redis'e başvurulmaz | İlk gün, kullanım, limit ve sürüm döner |

İki depo arasında ortak transaction veya hız hakkı iadesi yoktur. PostgreSQL'e ulaşmamış bir ön sonucun tekrarı Redis'in 24 saatlik kaydını kullanır. Arada politika güncellendiyse bu ön sonuç eski hız limitini korur; nihai PostgreSQL kararı günlük limit/sürümü o transaction'daki güncel politikadan alır. Kalıcılaştırılmış kararlar politika değişiminden etkilenmez. Redis veri kaybında tamamlanmamış ön sonuç devamlılığı garanti edilmez.

İki kopya ve middleware için yerel kabul:

```powershell
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg06.ps1
docker compose -p quotagate-qg06 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml down
```

QG-06 kendi `quotagate-qg06_pgdata` hacmini kullanır. Önceki demolarla aynı portları kullanır; sırayla çalıştırın. Birleşik mod yalnızca nihai karar ucunu açar; QG-05 ön kontrol ucu bu modda yoktur. Readiness PostgreSQL ve Redis'i kontrol eder. Redis erişilemese de mevcut PostgreSQL kararının tekrarı dönebilir; yeni kararda bağımlılık hatası `503` olur.

Betik dört geçici müşteri oluşturur. İki kopyaya 60 benzersiz çağrı günlük limit `3`, hız limiti `7` ile `3 allowed / 4 daily_quota / 53 rate_limited` verir. Yirmi aynı kimlikli çağrı bir günlük/hız hakkı kullanır. Bir müşteri için geçici PostgreSQL constraint'i, Redis kontrolünden sonraki karar yazımını bozarak `503` ve korunmuş Redis kapasitesini gösterir; aynı kimlikle tekrar sonucu tamamlar. Örnek `/jobs` için hız ve günlük retleri `429` olur, handler yalnızca iki izin için çalışır. PostgreSQL kullanım/karar/izin/günlük ret/hız ret toplamı `6|67|6|6|55` ile uzlaştırılır; loglarda sır aranır. `finally` geçici constraint'i, yalnızca kendi müşterilerini ve Redis anahtarlarını temizler. Başka trafik olmayan izole ortamda çalıştırın.

Birleşik entegrasyon testleri için migration uygulanmış izole PostgreSQL'i `QG_TEST_DATABASE_URL`, test Redis'ini `QG_TEST_REDIS_URL` ile verin. Her iki değişken varsa `go test ./... -count=1 -timeout=90s`, gerçek iki bağımlılıkta yarışları, Redis yanıt kaybını, PostgreSQL INSERT hatasında günlük artışın geri alınmasını, kalıcı tekrarın Redis'i atlamasını ve HTTP yanıt kaybını doğrular. Değişkenler eksikse ilgili entegrasyon testleri atlanır. Testler yalnızca rastgele oluşturdukları müşteri verisini temizler; hata enjeksiyonu kısa süreli, müşteriye özgü bir constraint eklediğinden paylaşılan/üretim veritabanında çalıştırmayın.

## QG-07 sınırlı tekrar ve handler sınırı

`Client.Decide`, tek karar çağrısında aynı `decision_id`, işlem, müşteri anahtarı ve gövdeyi korur. En fazla **üç deneme**, deneme başına **600 ms**, aralarda **50 ms / 100 ms** bekleme ve hepsini kapsayan **iki saniyelik toplam bütçe** vardır. Gelen bağlam daha erken sona ererse bütçeyi kısaltır. Bekleme ve yanıt gövdesi okuma da bütçeye dahildir; deneme sayısı veya bütçe dolunca `503` ve çalışmayan handler sonucu alınır.

| Sonuç | Tekrar |
| --- | --- |
| Bağlantı kopması, EOF/yarım HTTP gövdesi, deneme timeout'u | Bütçe ve deneme sayısı kaldıysa aynı kimlikle |
| HTTP `502`, `503`, `504` | Bütçe ve deneme sayısı kaldıysa aynı kimlikle |
| Nihai `allowed`, `daily_quota`, `rate_limited` | Yok; handler veya `429` |
| HTTP `401`, `403`, `409`, diğer HTTP durumları ve yönlendirme | Yok |
| TLS doğrulama hatası, bozuk/çelişkili/eksik/4096 bayttan büyük karar | Yok; handler çalışmaz |
| Gelen bağlamın iptali veya toplam bütçenin bitmesi | Yok; handler çalışmaz |

İlk deneme PostgreSQL'e commit edip yanıtını kaybettiyse tekrar kalıcı kararı bulur; günlük ve Redis hakkı tekrar harcanmaz. Bütün yanıtlar kaybolursa handler çalışmadan `503` dönebilir, fakat kalıcı hak ayrılmış olabilir. Aynı karar kimliğiyle manuel tekrar ilk sonucu gösterir. Ayrı bir `POST /jobs`, aynı gövde ve `Idempotency-Key` header'ına sahip olsa da yeni karar kimliği üretir; QuotaGate handler yan etkilerini tekilleştirmez. Bu sürümde her deneme aynı servis adresine gider; kopyalar arası failover istemcide uygulanmıyor.

Yerel Go araç zinciriyle iki Compose kopyasına karşı kabul:

```powershell
docker compose -p quotagate-qg07 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml up --build -d --wait
# Host Go modül önbelleği ilk kez hazırlanacaksa: go mod download
pwsh -NoProfile -File ./scripts/smoke-qg07.ps1
docker compose -p quotagate-qg07 --env-file .env.compose -f compose.yaml -f compose.qg05.yaml -f compose.qg06.yaml down
```

QG-07 ayrı `quotagate-qg07_pgdata` hacmi kullanır; önceki demolarla aynı portları kullandığından sırayla çalıştırın. Betik iki gerçek Compose API kopyasını kullanır; **hostta çalışan test proxy'si ve örnek API handler'ı**, commit edilmiş yanıtı düşürür veya bekletir. Üretim servisine hata enjeksiyonu ucu eklenmez. Proxy bu testte denemeleri iki kopyaya sırayla iletir; istemci tek proxy adresini görür. Bu test, servis adresi failover'ı veya gerçek bağımlılık kesintisinden toparlanma kanıtı değildir; kesinti demosu QG-08'dedir.

Kabul dört geçici müşteride ilk yanıt kaybından toparlanmayı, bütün yanıtların kaybında üç denemede kapanmayı, manuel kalıcı tekrarı, farklı inbound kimliklerini, kısa caller deadline'ını ve toplam iki saniyelik bütçeyi doğrular. `/demo/stats` handler sayacı ve her müşteri için PostgreSQL kullanım/karar/izin sayıları ile Redis kapasitesi uzlaştırılır. Test, yalnızca kendi müşteri/Redis verisini ve yerel test sunucularını temizler; betik uygulama loglarında sır arar ve test env değerlerini eski haline getirir.

`go test ./client -count=1` bağlantı/gövde kaybı, `502/503/504`, timeout/iptal, terminal cevaplar ve 30 eşzamanlı inbound istekte 60 deneme/30 ayrı karar kimliğini doğrular. `QG_TEST_DATABASE_URL` ve `QG_TEST_REDIS_URL` varsa `TestExampleRetryAfterCommitWithPostgres`, gerçek iki bağımlılığa bağlı handler ile commit sonrası kaybı test eder. `TestQG07ComposeRetry` ise ayrıca iki `QG_TEST_API_URL`/`QG_TEST_API_URL_2` ve `QG_TEST_ADMIN_TOKEN` ister; betik bunları bellekte ayarlar. Canlı Compose testi Redis DB `0`, genel entegrasyon/race koşusu izole test DB `15` kullanır.

## QG-08 iki kopyalı kesinti ve toparlanma

`compose.qg08.yaml`, iki `serve-combined` kopyasını aynı PostgreSQL ve Redis'e bağlar; örnek API birinci kopyayı kullanır. Temel Compose dosyası üzerine tek ek yeterlidir:

```powershell
docker compose -p quotagate-qg08 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml up --build -d --wait
pwsh -NoProfile -File ./scripts/smoke-qg08.ps1
docker compose -p quotagate-qg08 --env-file .env.compose -f compose.yaml -f compose.qg08.yaml down
```

QG-08 ayrı `quotagate-qg08_pgdata` hacmini kullanır. Varsayılan portlar QuotaGate `18080/18082`, örnek API `18081`, PostgreSQL `55432`, Redis `56379`; hepsi loopback üzerinden açılır. Önceki demolarla aynı portları kullandığından ortamları sırayla çalıştırın. **Boş, izole test PostgreSQL/Redis'i ve başka trafik olmayan ortamı kullanın.** Betik başlangıçta müşteri/sayaç/karar tablolarının ve Redis demo DB'sinin boş olduğunu kontrol eder. Redis durdurulup başlatılacağı için bu ortamda başka veri tutulmamalıdır.

Üç geçici müşteriyle ortak limitler ve toparlanma sınanır:

| Müşteri | Günlük / dakika limiti | İki kopyaya eşzamanlı çağrı | Beklenen nihai sonuç |
| --- | --- | --- | --- |
| Ortak hız | `1000 / 7` | `60` benzersiz karar | `7 allowed / 53 rate_limited` |
| Ortak günlük kota | `3 / 1000` | `40` benzersiz karar | `3 allowed / 37 daily_quota` |
| Toparlanma | `4 / 1000` | Örnek API ve doğrudan kararlar | Kesintilerde handler çalışmaz; toparlanınca günlük kullanım `4` olur |

Yük iki QuotaGate kopyasına dönüşümlü dağıtılır. Hız testi gerçek Redis dakika penceresinde çalışır; günlük kota PostgreSQL'de paylaşılır. Sonra betik yalnızca bağımlılık konteynerlerini durdurup başlatır:

| Kesinti | İki kopyada live / ready | Yeni karar ve örnek `/jobs` | Kayıtlı kararın tekrarı | Handler ve kalıcı sayaçlar |
| --- | --- | --- | --- | --- |
| Redis kapalı | `200 / 503` | `503` | PostgreSQL'den `200`, ilk sonuç korunur | Artmaz |
| PostgreSQL kapalı | `200 / 503` | `503` | `503` | Artmaz; DB geri gelince önceki kayıtlar korunur |

Redis bu demoda disk kalıcılığı olmadan çalışır. Yeniden başlatma hız sayaçlarını ve ön sonuçları kaybettirir. PostgreSQL'deki nihai kararın tekrarı Redis'i doldurmaz; yeni karar yeniden hız kapasitesi kullanır. Bu kabul hız verisinin kesinti boyunca korunmasını vaat etmez. PostgreSQL **çağrılardan önce tamamen durdurulduğunda** kimlik doğrulama başarısız olur ve yeni Redis ön sonucu oluşmaz. Redis kontrolünden sonra yaşanan DB hatasında kapasite tüketilmiş olabilir; bu ayrı hata noktası QG-06 kabulünde doğrulandı.

İki bağımlılık geri geldiğinde readiness ve yeni kararlar toparlanır. Betik iki QuotaGate kopyasının ve örnek API'nin konteyner kimliği, başlangıç zamanı ve restart sayısını karşılaştırarak uygulamalar yeniden başlatılmadan bağlantıların toparlandığını doğrular. Son günlük kullanım müşteri bazında `7/3/4`; PostgreSQL kullanım/karar/izin/günlük ret/hız ret/farklı kimlik toplamı `14|105|14|38|53|105` olur. Örnek API handler'ı üç izin için `+3` çalışır; son günlük kota reddi `429` verir. Doğrudan karar izinleri handler çağrısı değildir.

Betik loglarda müşteri anahtarlarını, yönetici sırrını ve DB parolasını arar. `finally` durdurduğu bağımlılıkları geri getirir, yalnızca oluşturduğu müşteri verisini ve Redis anahtarlarını temizler. `down` PostgreSQL hacmini korur. Bu yerel demo üretim HA veya otomatik failover kanıtı değildir. Birim, gerçek bağımlılık, race ve canlı Compose kabul sonuçları [STATUS.md](STATUS.md) içindedir; sıradaki görev QG-09 gözlemlenebilir kararlardır.
