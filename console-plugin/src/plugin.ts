export const extensions = [
  {
    type: 'console.navigation/href',
    properties: {
      id: 'ahv-to-ove-plugin',
      perspective: 'admin',
      section: 'virtualization',
      // @ts-ignore
      name: 'AHV 移行',
      href: '/ahv-to-ove-plugin',
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/ahv-to-ove-plugin',
      component: { $codeRef: 'MigrationListPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/ahv-to-ove-plugin/ns/:namespace/:name',
      component: { $codeRef: 'MigrationDetailPage' },
    },
  },
  {
    type: 'console.page/route',
    properties: {
      exact: true,
      path: '/ahv-to-ove-plugin/create',
      component: { $codeRef: 'MigrationCreatePage' },
    },
  },
];
