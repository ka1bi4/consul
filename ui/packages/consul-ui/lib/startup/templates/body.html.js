// rootURL in production equals `{{.ContentPath}}` and therefore is replaced
// with the value of -ui-content-path. During development rootURL uses the
// value as set in environment.js

const read = require('fs').readFileSync;

const hbsRe = /{{(@[a-z]*)}}/g;

const hbs = (path, attrs = {}) =>
  read(`${process.cwd()}/app/components/${path}`)
    .toString()
    .replace('{{yield}}', '')
    .replace(hbsRe, (match, prop) => attrs[prop.substr(1)]);

const BrandLoader = attrs => hbs('brand-loader/index.hbs', attrs);
const Enterprise = attrs => hbs('brand-loader/enterprise.hbs', attrs);

module.exports = ({ appName, environment, rootURL, config }) => `
  <noscript>
      <div style="margin: 0 auto;">
          <h2>JavaScript Required</h2>
          <p>Please enable JavaScript in your web browser to use Consul UI.</p>
      </div>
  </noscript>
${BrandLoader({
  color: '#8E96A3',
  width: config.CONSUL_BINARY_TYPE !== 'oss' && config.CONSUL_BINARY_TYPE !== '' ? `394` : `198`,
  subtitle:
    config.CONSUL_BINARY_TYPE !== 'oss' && config.CONSUL_BINARY_TYPE !== '' ? Enterprise() : ``,
})}
  <script type="application/json" data-consul-ui-config>
${environment === 'production' ? `{{jsonEncode .}}` : JSON.stringify(config.operatorConfig)}
  </script>
  <script type="application/json" data-consul-ui-fs>
  {
    "text-encoding/encoding-indexes.js": "${rootURL}assets/encoding-indexes.js",
    "text-encoding/encoding.js": "${rootURL}assets/encoding.js",
    "css.escape/css.escape.js": "${rootURL}assets/css.escape.js",
    "codemirror/mode/javascript/javascript.js": "${rootURL}assets/codemirror/mode/javascript/javascript.js",
    "codemirror/mode/ruby/ruby.js": "${rootURL}assets/codemirror/mode/ruby/ruby.js",
    "codemirror/mode/yaml/yaml.js": "${rootURL}assets/codemirror/mode/yaml/yaml.js"
  }
  </script>
  <script data-app-name="${appName}" data-${appName}-services src="${rootURL}assets/consul-ui/services.js"></script>
${
  environment === 'development' || environment === 'staging'
    ? `
  <script data-app-name="${appName}" data-${appName}-services src="${rootURL}assets/consul-ui/services-debug.js"></script>
` : ``}
${
  environment === 'production'
    ? `
{{if .ACLsEnabled}}
  <script data-app-name="${appName}" data-${appName}-routing src="${rootURL}assets/consul-acls/routes.js"></script>
{{end}}
{{if .PartitionsEnabled}}
  <script data-app-name="${appName}" data-${appName}-routing src="${rootURL}assets/consul-partitions/routes.js"></script>
{{end}}
`
    : `
<script>
(
  function(get, obj) {
    Object.entries(obj).forEach(([key, value]) => {
      if(get(key)) {
        const appName = '${appName}';
        const appNameJS = appName.split('-').map((item, i) => i ? \`\${item.substr(0, 1).toUpperCase()}\${item.substr(1)}\` : item).join('');
        const $script = document.createElement('script');
        $script.setAttribute('data-app-name', '${appName}');
        $script.setAttribute('data-${appName}-routing', '');
        $script.setAttribute('src', \`${rootURL}assets/\${value}/routes.js\`);
        document.body.appendChild($script);
      }
    });
  }
)(
  key => document.cookie.split('; ').find(item => item.startsWith(\`\${key}=\`)),
  {
    'CONSUL_ACLS_ENABLE': 'consul-acls',
    'CONSUL_PARTITIONS_ENABLE': 'consul-partitions'
  }
);
</script>
`
}
  <script src="${rootURL}assets/init.js"></script>
  <script src="${rootURL}assets/vendor.js"></script>
  ${environment === 'test' ? `<script src="${rootURL}assets/test-support.js"></script>` : ``}
  <script src="${rootURL}assets/metrics-providers/consul.js"></script>
  <script src="${rootURL}assets/metrics-providers/prometheus.js"></script>
  ${
    environment === 'production'
      ? `{{ range .ExtraScripts }} <script src="{{.}}"></script> {{ end }}`
      : ``
  }
  <script src="${rootURL}assets/${appName}.js"></script>
  ${environment === 'test' ? `<script src="${rootURL}assets/tests.js"></script>` : ``}
`;
